package main

import (
	"flag"
	"fmt"
	"k8s.io/kubernetes/pkg/client/unversioned"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/golang/glog"
	"github.com/jenkins-x/exposecontroller/controller"
	"github.com/jenkins-x/exposecontroller/version"
	"github.com/spf13/pflag"
	"k8s.io/kubernetes/pkg/api"
	kubectlutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"
)

const (
	healthPort = 10254
)

var (
	flags = pflag.NewFlagSet("", pflag.ExitOnError)

	configFile = flags.String("config", "/etc/exposecontroller/config.yml",
		`Path to the file that contains the exposecontroller configuration to use`)

	resyncPeriod = flags.Duration("sync-period", 30*time.Second,
		`Relist and confirm services this often.`)

	healthzPort = flags.Int("healthz-port", healthPort, "port for healthz endpoint.")

	profiling = flags.Bool("profiling", true, `Enable profiling via web interface host:port/debug/pprof/`)

	daemon  = flag.Bool("daemon", false, `Run as daemon mode watching changes as it happens.`)
	cleanup = flag.Bool("cleanup", false, `Removes Ingress rules that were generated by exposecontroller`)

	domain                = flag.String("domain", "", "Domain to use with your DNS provider (default: .nip.io).")
	filter                = flag.String("filter", "", "The filter of service names to look for when cleaning up")
	exposer               = flag.String("exposer", "", "Which strategy exposecontroller should use to access applications")
	apiserver             = flag.String("api-server", "", "API server URL")
	consoleurl            = flag.String("console-server", "", "Console URL")
	httpb                 = flag.Bool("http", false, `Use HTTP`)
	watchNamespaces       = flag.String("watch-namespace", "", "Exposecontroller will only look at the provided namespace")
	watchCurrentNamespace = flag.Bool("watch-current-namespace", true, `Exposecontroller will look at the current namespace only - (default: 'true' unless --watch-namespace specified)`)
	services              = flag.String("services", "", "List of comma separated service names which will be exposed, if empty all services from namespace will be considered")
)

func main() {
	factory := kubectlutil.NewFactory(nil)
	factory.BindFlags(flags)
	factory.BindExternalFlags(flags)
	flags.Parse(os.Args)
	flag.CommandLine.Parse([]string{})

	glog.Infof("Using build: %v", version.Version)

	kubeClient, err := factory.Client()
	if err != nil {
		glog.Fatalf("failed to create client: %s", err)
	}
	currentNamespace := os.Getenv("KUBERNETES_NAMESPACE")
	if len(currentNamespace) == 0 {
		currentNamespace, _, err = factory.DefaultNamespace()
		if err != nil {
			glog.Fatalf("Could not find the current namespace: %v", err)
		}
	}

	restClientConfig, err := factory.ClientConfig()
	if err != nil {
		glog.Fatalf("failed to create REST client config: %s", err)
	}

	controllerConfig, exists, err := controller.LoadFile(*configFile)
	if !exists || err != nil {
		if err != nil {
			glog.Warningf("failed to load config file: %s", err)
		}

		cc2 := tryFindConfig(kubeClient, currentNamespace)
		if cc2 == nil {
			// lets try find the ConfigMap in the dev namespace
			resource, err := kubeClient.Namespaces().Get(currentNamespace)
			if err == nil && resource != nil {
				labels := resource.Labels
				if labels != nil {
					ns := labels["team"]
					if ns == "" {
						glog.Warningf("No 'team' label on Namespace %s", currentNamespace)
					} else {
						glog.Infof("trying to find the ConfigMap in the Dev Namespace %s", ns)

						cc2 = tryFindConfig(kubeClient, ns)
					}
				} else {
					glog.Warningf("No labels on Namespace %s", currentNamespace)
				}
			} else {
				glog.Warningf("Failed to load Namespace %s: %s", currentNamespace, err)
			}
		}
		if cc2 != nil {
			controllerConfig = cc2
		}
	} else {
		glog.Infof("Loaded config file %s", *configFile)
	}
	glog.Infof("Config file before overrides %s", controllerConfig.String())

	if *domain != "" {
		controllerConfig.Domain = *domain
	}
	if *exposer != "" {
		controllerConfig.Exposer = *exposer
	}
	if *apiserver != "" {
		controllerConfig.ApiServer = *apiserver
	}
	if *consoleurl != "" {
		controllerConfig.ConsoleURL = *consoleurl
	}
	if *httpb {
		controllerConfig.HTTP = *httpb
	}

	if *watchCurrentNamespace {
		controllerConfig.WatchCurrentNamespace = *watchCurrentNamespace
	}
	if *watchNamespaces != "" {
		controllerConfig.WatchNamespaces = *watchNamespaces
		controllerConfig.WatchCurrentNamespace = false
	}

	if *services != "" {
		controllerConfig.Services = strings.Split(*services, ",")
	}

	glog.Infof("Config file after overrides %s", controllerConfig.String())

	//watchNamespaces := api.NamespaceAll
	watchNamespaces := controllerConfig.WatchNamespaces
	if controllerConfig.WatchCurrentNamespace {
		if len(currentNamespace) == 0 {
			glog.Fatalf("No current namespace found!")
		}
		watchNamespaces = currentNamespace
	}

	if *cleanup {
		ingress, err := kubeClient.Ingress(watchNamespaces).List(api.ListOptions{})
		if err != nil {
			glog.Fatalf("Could not get ingress rules in namespace %s %v", watchNamespaces, err)
		}

		for _, i := range ingress.Items {
			if i.Annotations["fabric8.io/generated-by"] == "exposecontroller" {
				if filter == nil || strings.Contains(i.Name, *filter) {
					glog.Infof("Deleting ingress %s", i.Name)
					err := kubeClient.Ingress(watchNamespaces).Delete(i.Name, nil)
					if err != nil {
						glog.Fatalf("Could not find the current namespace: %v", err)
					}
				}
			}
		}
		return
	}

	if *daemon {
		glog.Infof("Watching services in namespaces: `%s`", watchNamespaces)

		c, err := controller.NewController(kubeClient, restClientConfig, factory.JSONEncoder(), *resyncPeriod, watchNamespaces, controllerConfig)
		if err != nil {
			glog.Fatalf("%s", err)
		}

		go registerHandlers()
		go handleSigterm(c)

		c.Run()
	} else {
		glog.Infof("Running in : `%s`", watchNamespaces)
		c, err := controller.NewController(kubeClient, restClientConfig, factory.JSONEncoder(), *resyncPeriod, watchNamespaces, controllerConfig)
		if err != nil {
			glog.Fatalf("%s", err)
		}

		ticker := time.NewTicker(5 * time.Second)
		quit := make(chan struct{})
		go func() {
			for {
				select {
				case <-ticker.C:
					if c.Hasrun() {
						close(quit)
					}
				case <-quit:
					c.Stop()
					ticker.Stop()
					return
				}
			}
		}()
		// Handle Control-C has well here
		go handleSigterm(c)

		c.Run()
	}
}

func tryFindConfig(kubeClient *unversioned.Client, ns string) *controller.Config {
	var controllerConfig *controller.Config
	cm, err := kubeClient.ConfigMaps(ns).Get("exposecontroller")
	if err == nil {
		glog.Infof("Using ConfigMap exposecontroller to load configuration...")
		// TODO we could allow the config to be passed in via key/value pairs?
		text := cm.Data["config.yml"]
		if text != "" {
			controllerConfig, err = controller.Load(text)
			if err != nil {
				glog.Warningf("Could not parse the config text from exposecontroller ConfigMap  %v", err)
			}
			glog.Infof("Loaded ConfigMap exposecontroller to load configuration!")
		}
	} else {
		glog.Warningf("Could not find ConfigMap exposecontroller ConfigMap in namespace %s", ns)

		cm, err = kubeClient.ConfigMaps(ns).Get("ingress-config")
		if err != nil {
			glog.Warningf("Could not find ConfigMap ingress-config ConfigMap in namespace %s", ns)
		} else {
			glog.Infof("Loaded ConfigMap ingress-config to load configuration!")
			data := cm.Data
			if data != nil {
				controllerConfig, err = controller.MapToConfig(data)
				if err != nil {
					glog.Warningf("Failed to convert Map data %#v from configMap ingress-config in namespace %s due to: %s\n", controllerConfig, ns, err)
				}
			}
		}
	}
	return controllerConfig
}

func registerHandlers() {
	mux := http.NewServeMux()

	if *profiling {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	}

	server := &http.Server{
		Addr:    fmt.Sprintf(":%v", *healthzPort),
		Handler: mux,
	}
	glog.Fatal(server.ListenAndServe())
}

func handleSigterm(c *controller.Controller) {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)
	sig := <-signalChan
	glog.Infof("Received %s, shutting down", sig)
	c.Stop()
}
