/*
Copyright 2019 Sergio Rua

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"regexp"
	"strings"

	log "github.com/sirupsen/logrus"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"

	"gopkg.in/alecthomas/kingpin.v2"

	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"

	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type ctxKey string

var (
	version         = "snapshot"
	debug           = kingpin.Flag("debug", "Enable debug logging").Bool()
	local           = kingpin.Flag("local", "Run locally for development").Bool()
	kubeconfig      = kingpin.Flag("kubeconfig", "Path to kubeconfig").OverrideDefaultFromEnvar("KUBECONFIG").String()
	dryrun          = kingpin.Flag("dry-run", "Don't actually tag the volumes").Bool()
	ns              = ctxKey("namespace")
	pvcname         = ctxKey("volumeClaimName")
	volname         = ctxKey("volumeName")
	eventsProcessed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "kubetagger_processed_events_total",
		Help: "The total number of processed events",
	})
	tagsAdded = promauto.NewCounter(prometheus.CounterOpts{
		Name: "kubetagger_volume_tags_added",
		Help: "Number of tags added to volumes the kubetagger",
	})
	tagsExisting = promauto.NewCounter(prometheus.CounterOpts{
		Name: "kubetagger_volume_tags_existing",
		Help: "Number of tags already existing on volumes",
	})
	volumesTagged = promauto.NewCounter(prometheus.CounterOpts{
		Name: "kubetagger_volumes_tagged",
		Help: "Number of volumes tagged by kubetagger",
	})
	processingErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "kubetagger_errors",
		Help: "Number of errors while processing",
	})
)

func init() {
	kingpin.Version(version)
	kingpin.Parse()
}

func logWithCtx(ctx context.Context) *log.Entry {

	entry := log.WithContext(ctx)

	if ns := ctx.Value(ns); ns != nil {
		entry = entry.WithField("namespace", ns)
	}
	if pvcname := ctx.Value(pvcname); pvcname != nil {
		entry = entry.WithField("volumeClaimName", pvcname)
	}
	if volname := ctx.Value(volname); volname != nil {
		entry = entry.WithField("volumeName", volname)
	}
	return entry
}

func main() {

	http.Handle("/metrics", promhttp.Handler())
	go func() {
		http.ListenAndServe(":2112", nil)
	}()

	ctx := context.Background()

	var config *rest.Config
	var err error

	if !*local {
		config, err = rest.InClusterConfig()
		if err != nil {
			log.WithError(err).Fatal("Error building In Cluster Config")
		}
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			log.WithError(err).Fatalf("Error creating config from kubeconfig: %s", *kubeconfig)
		}
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		logWithCtx(ctx).WithError(err).Fatal("Error creating clientset")
	}

	watcher, err := clientset.CoreV1().PersistentVolumeClaims("").Watch(metav1.ListOptions{})
	if err != nil {
		logWithCtx(ctx).WithError(err).Fatal("Error creating PVC watcher")
	}
	/* changes */
	ch := watcher.ResultChan()

	for event := range ch {
		eventsProcessed.Inc()
		pvc, ok := event.Object.(*v1.PersistentVolumeClaim)
		if !ok {
			logWithCtx(ctx).Fatal("Unexpected event type")
		}
		if event.Type == watch.Added || event.Type == watch.Modified {
			namespace := pvc.GetNamespace()
			volumeClaimName := pvc.GetName()
			volumeClaim := *pvc
			volumeName := volumeClaim.Spec.VolumeName

			ctx = context.WithValue(ctx, ns, namespace)
			ctx = context.WithValue(ctx, pvcname, volumeClaimName)
			ctx = context.WithValue(ctx, volname, volumeName)

			awsVolume, errp := clientset.CoreV1().PersistentVolumes().Get(volumeName, metav1.GetOptions{})
			if errp != nil {
				logWithCtx(ctx).WithError(errp).Error("Cannot find EBS volume associated with Volume Claim")
				processingErrors.Inc()
				continue
			}
			awsVolumeID := awsVolume.Spec.PersistentVolumeSource.AWSElasticBlockStore.VolumeID
			logWithCtx(ctx).Info("Processing Volume Tags")
			if isEBSVolume(&volumeClaim) {
				separator := ","
				tagsToAdd := ""
				for k, v := range volumeClaim.Annotations {
					if k == "volume.beta.kubernetes.io/additional-resource-tags-separator" {
						separator = v
					}

					if k == "volume.beta.kubernetes.io/additional-resource-tags" {
						tagsToAdd = v
					}
				}
				if tagsToAdd != "" {
					if !*dryrun {
						addAWSTags(ctx, tagsToAdd, awsVolumeID, separator)
					} else {
						logWithCtx(ctx).WithFields(log.Fields{"tags": tagsToAdd, "volId": awsVolumeID}).Info("Running in dry run mode, not adding tags")
					}

				}
			} else {
				logWithCtx(ctx).Warn("Volume is not EBS. Ignoring")
			}
		}
	}
}

/*
	This only works for EBS volumes. Make sure they are!
*/
func isEBSVolume(volume *v1.PersistentVolumeClaim) bool {
	for k, v := range volume.Annotations {
		if k == "volume.beta.kubernetes.io/storage-provisioner" && v == "kubernetes.io/aws-ebs" {
			return true
		}
	}
	return false
}

/*
	Loops through the tags found for the volume and calls `setTag`
	to add it via the AWS api
*/
func addAWSTags(ctx context.Context, awsTags string, awsVolumeID string, separator string) {

	var tagAdded = false
	awsRegion, awsVolume := splitVol(awsVolumeID)

	/* Connect to AWS */
	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(awsRegion),
	})
	if err != nil {
		panic(err)
	}

	svc := ec2.New(sess)

	params := &ec2.DescribeVolumesInput{
		VolumeIds: []*string{&awsVolume},
	}

	tags := strings.Split(awsTags, separator)

	resp, err := svc.DescribeVolumes(params)
	if err != nil {
		logWithCtx(ctx).WithError(err).WithFields(log.Fields{"volId": awsVolume, "region": awsRegion}).Error("Cannot get volume")
		processingErrors.Inc()
		return
	}
	for i := range tags {
		t := strings.Split(tags[i], "=")
		if len(t) != 2 {
			logWithCtx(ctx).Error("Skipping malformed tag")
			processingErrors.Inc()
			continue
		}
		logWithCtx(ctx).WithFields(log.Fields{"tagKey": t[0], "tagValue": t[1], "volId": awsVolume, "region": awsRegion}).Info("Processing EBS Volume")
		if !hasTag(ctx, resp.Volumes[0].Tags, t[0], t[1], awsVolume, awsRegion) {
			tagAdded = true
			setTag(ctx, svc, t[0], t[1], awsVolume)
		}
	}
	if tagAdded {
		volumesTagged.Inc()
	}
}

/*
	AWS api call to set the tag found in the annotations
*/
func setTag(ctx context.Context, svc *ec2.EC2, tagKey string, tagValue string, volumeID string) bool {
	tags := &ec2.CreateTagsInput{
		Resources: []*string{
			aws.String(volumeID),
		},
		Tags: []*ec2.Tag{
			{
				Key:   aws.String(tagKey),
				Value: aws.String(tagValue),
			},
		},
	}
	ret, err := svc.CreateTags(tags)
	if err != nil {
		logWithCtx(ctx).WithError(err).WithFields(log.Fields{"volId": volumeID}).Fatal("Error creating tags")
		return false
	}
	if *debug {
		logWithCtx(ctx).Debugf("Returned value from CreatesTags call: %v", ret)
	}
	tagsAdded.Inc()
	return true
}

/*
   Check if the tag is already set. It wouldn't be a problem if it is
   but if you're using cloudtrail it may be an issue seeing it
   being set all muiltiple times
*/
func hasTag(ctx context.Context, tags []*ec2.Tag, key string, value string, awsVolume string, awsRegion string) bool {
	for i := range tags {
		if *tags[i].Key == key && *tags[i].Value == value {
			logWithCtx(ctx).WithFields(log.Fields{"tagKey": key, "tagValue": value, "volId": awsVolume, "region": awsRegion}).Info("Tag value already exists")
			tagsExisting.Inc()
			return true
		}
	}
	return false
}

/* Take a URL as returned by Kubernetes in the format

aws://eu-west-1b/vol-7iyw8ygidg

and returns region and volume name
*/
func splitVol(vol string) (string, string) {
	sp := strings.Split(vol, "/")
	re := regexp.MustCompile(`[a-z]$`)
	zone := re.ReplaceAllString(sp[2], "")

	return zone, sp[3]
}
