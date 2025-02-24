// Copyright 2018 Tetrate Labs
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	ic "istio.io/client-go/pkg/clientset/versioned"
	icinformer "istio.io/client-go/pkg/informers/externalversions/networking/v1alpha3"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/tetratelabs/istio-registry-sync/pkg/cloudmap"
	"github.com/tetratelabs/istio-registry-sync/pkg/consul"
	"github.com/tetratelabs/istio-registry-sync/pkg/control"
	"github.com/tetratelabs/istio-registry-sync/pkg/provider"
	"github.com/tetratelabs/istio-registry-sync/pkg/serviceentry"
	"github.com/tetratelabs/log"
)

const (
	apiGroup      = "networking.istio.io"
	apiVersion    = "v1alpha3"
	apiType       = apiGroup + "/" + apiVersion
	kind          = "ServiceEntry"
	allNamespaces = ""
)

var (
	id              string
	debug           bool
	kubeConfig      string
	namespace       string
	awsRegion       string
	awsID           string
	awsSecret       string
	consulEndpoint  string
	consulNamespace string
	resyncPeriod int
)

func serve() (serve *cobra.Command) {

	serve = &cobra.Command{
		Use:     "serve",
		Aliases: []string{"serve"},
		Short:   "Starts the Istio Cloud Map Operator server",
		Example: "istio-registry-sync serve --id 123",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := clientcmd.BuildConfigFromFlags("", kubeConfig)
			if err != nil {
				return errors.Wrapf(err, "failed to create a kube client from the config %q", kubeConfig)
			}
			ic, err := ic.NewForConfig(cfg)
			if err != nil {
				return errors.Wrap(err, "failed to create an istio client from the k8s rest config")
			}

			t := true
			sessionUUID := uuid.NewUUID()
			owner := v1.OwnerReference{
				APIVersion: "cloudmap.istio.io",
				Kind:       "ServiceController",
				Name:       id,
				Controller: &t,
				UID:        sessionUUID,
			}

			// TODO: move over to run groups, get a context there to use to handle shutdown gracefully.
			ctx := context.Background() // common context for cancellation across all loops/routines

			// TODO: see if we can push down into the istio setup section
			if len(namespace) == 0 {
				if ns, set := os.LookupEnv("PUBLISH_NAMESPACE"); set {
					namespace = ns
				}
			}

			watcher, err := getWatcher(ctx)
			if err != nil {
				return err
			}

			go watcher.Run(ctx)
			istio := serviceentry.New(owner)
			if debug {
				istio = serviceentry.NewLoggingStore(istio, log.Infof)
			}
			log.Info("Starting Synchronizer control loop")

			// we get the service entry for namespace `namespace` for the synchronizer to publish service entries in to
			// (if we use an `allNamespaces` client here we can't publish). Listening for ServiceEntries is done with
			// the informer, which uses allNamespace.
			write := ic.NetworkingV1alpha3().ServiceEntries(findNamespace(namespace))
			sync := control.NewSynchronizer(owner, istio, watcher.Store(), watcher.Prefix(), write)
			go sync.Run(ctx)

			informer := icinformer.NewServiceEntryInformer(ic, allNamespaces, time.Duration(resyncPeriod)*time.Second,
				// taken from https://github.com/istio/istio/blob/release-1.5/pilot/pkg/bootstrap/namespacecontroller.go
				cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			serviceentry.AttachHandler(istio, informer)
			log.Infof("Watching %s.%s across all namespaces with resync period %d and id %q", apiType, kind, resyncPeriod, id)
			informer.Run(ctx.Done())
			return nil
		},
	}

	serve.PersistentFlags().StringVar(&id,
		"id", "istio-registry-sync-operator", "ID of this instance; instances will only ServiceEntries marked with their own ID.")
	serve.PersistentFlags().BoolVar(&debug, "debug", true, "if true, enables more logging")
	serve.PersistentFlags().StringVar(&kubeConfig,
		"kube-config", "", "kubeconfig location; if empty the server will assume it's in a cluster; for local testing use ~/.kube/config")
	serve.PersistentFlags().StringVar(&namespace, "namespace", "",
		"If provided, the namespace this operator publishes ServiceEntries to. If no value is provided it will be populated from the PUBLISH_NAMESPACE environment variable. If both are empty, the operator will publish into the namespace it is deployed in")

	serve.PersistentFlags().StringVar(&awsRegion, "aws-region", "",
		"AWS Region to connect to Cloud Map. Use this OR the environment variable AWS_REGION.")
	serve.PersistentFlags().StringVar(&awsID, "aws-access-key-id", "",
		"AWS Access Key ID to use to connect to Cloud Map. Use flags for both this and --aws-secret-access-key OR use "+
			"the environment variables AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY. Flags and env vars cannot be mixed.")
	serve.PersistentFlags().StringVar(&awsSecret, "aws-secret-access-key", "",
		"AWS Secret Access Key to use to connect to Cloud Map. Use flags for both this and --aws-access-key-id OR use "+
			"the environment variables AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY. Flags and env vars cannot be mixed.")
	serve.PersistentFlags().StringVar(&consulEndpoint, "consul-endpoint", "",
		"Consul's endpoint to query service catalog. This must include its scheme http// or https//. (e.g. http://localhost:8500)")
	serve.PersistentFlags().StringVar(&consulNamespace, "consul-namespace", "",
		"Consul's namespace to search service catalog")
	serve.PersistentFlags().IntVar(&resyncPeriod, "resync-period", 5, "Time in seconds between resyncs")
	return serve
}

func getWatcher(ctx context.Context) (provider.Watcher, error) {
	store := provider.NewStore()
	log.Info("Initializing Watchers")
	cmWatcher, awsErr := cloudmap.NewWatcher(ctx, store, awsRegion, awsID, awsSecret)
	if awsErr == nil {
		log.Infof("Cloud Map Watcher initialized in %q", awsRegion)
	}
	consulWatcher, consulErr := consul.NewWatcher(store, consulEndpoint, consulNamespace)
	if consulErr == nil {
		log.Infof("Consul Watcher initialized at %s", consulEndpoint)
	}

	var watcher provider.Watcher
	if awsErr != nil && consulErr != nil {
		log.Errorf("error setting up aws: %v", awsErr)
		log.Errorf("error setting up consul: %v", consulErr)
		return nil, errors.New("failed to initialize watchers")
	} else if awsErr == nil {
		// here we prioritize Cloud Map over Consul.
		// TODO: should we support both sources simultaneously?
		//   To do so, we need to modify synchronizer to support multiple Stores
		watcher = cmWatcher
	} else {
		watcher = consulWatcher
	}

	return watcher, nil
}

func main() {
	root := &cobra.Command{
		Short:   "istio-registry-sync",
		Example: "",
	}
	// TODO: add other commands for listing services under management, etc.
	root.AddCommand(serve())
	if err := root.Execute(); err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}
}

func findNamespace(namespace string) string {
	if len(namespace) > 0 {
		log.Infof("using namespace flag to publish service entries into %q", namespace)
		return namespace
	}
	// This way assumes you've set the POD_NAMESPACE environment variable using the downward API.
	// This check has to be done first for backwards compatibility with the way InClusterConfig was originally set up
	if ns, ok := os.LookupEnv("POD_NAMESPACE"); ok {
		log.Infof("using POD_NAMESPACE environment variable to publish service entries into %q", namespace)
		return ns
	}

	// Fall back to the namespace associated with the service account token, if available
	if data, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(data)); len(ns) > 0 {
			log.Infof("using service account namespace from pod filesystem to publish service entries into %q", namespace)
			return ns
		}
	}

	log.Infof("couldn't determine a namespace, falling back to %q", "default")
	return "default"
}
