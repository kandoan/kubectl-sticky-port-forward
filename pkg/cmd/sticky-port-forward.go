package cmd

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/klog/v2"

	coreV1 "k8s.io/api/core/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/util/workqueue"

	"regexp"
)

var example = `
	# port-forward based on label
	%[1]s %[2]s --labels app=appname 80:8080

	# port-forward based on label with multiple ports
	%[1]s %[2]s --labels app=appname 80:8080 443:443

	# port-forward based on label with specific namespace
	%[1]s %[2]s --labels app=appname -n othernamespace 80:8080

	# port-forward based on pod name with regex matching
	%[1]s %[2]s --pod "pod_prefix.*" 80:8080

	# save params to a new yaml file to reuse later
	%[1]s %[2]s --labels app=appname 80:8080 -o filename.yml

	# save params to an existing yaml file to reuse later
	%[1]s %[2]s --labels app=appname 80:8080 -a filename.yml

	# use params from an existing yaml file
	%[1]s %[2]s -f filename.yml

	* If there are multiple pods matched, it will only use the first one.
`

// the running context of the app
type Context struct {
	streams      genericiooptions.IOStreams
	clientConfig *rest.Config
	clientset    *kubernetes.Clientset

	podIndexer        cache.Indexer
	podQueue          workqueue.DelayingInterface
	controllerReadyCh chan bool
	stopControllerCh  chan struct{}

	portForwardContexts []*PortForwardContext
}

// all options to check for a pod to port-forward to
type PortForwardOptions struct {
	NamespaceSelector string            `yaml:"namespace,omitempty"`
	PodSelector       string            `yaml:"podName,omitempty"`
	LabelSelector     map[string]string `yaml:"labels,omitempty"`

	Ports []string `yaml:"ports"`
}

func (options PortForwardOptions) String() string {
	var sb strings.Builder

	if options.NamespaceSelector != "" {
		sb.WriteString(fmt.Sprintf("Namespace=%s ", options.NamespaceSelector))
	}

	if options.PodSelector != "" {
		sb.WriteString(fmt.Sprintf("Pod=%s ", options.PodSelector))
	}

	if len(options.LabelSelector) > 0 {
		sb.WriteString("Label=[")

		isFirst := true
		for key, val := range options.LabelSelector {
			if !isFirst {
				sb.WriteString(" ")
			}
			sb.WriteString(fmt.Sprintf("%s=%s", key, val))
			isFirst = false
		}

		sb.WriteString("] ")
	}

	sb.WriteString("Ports=[")
	isFirst := true
	for _, val := range options.Ports {
		if !isFirst {
			sb.WriteString(" ")
		}
		sb.WriteString(val)
		isFirst = false
	}

	sb.WriteString("]")

	return sb.String()
}

// the context to port-forward
type PortForwardContext struct {
	options PortForwardOptions

	targetPodCh   chan *TargetPod
	stopForwardCh chan struct{}
	runningPod    atomic.Pointer[TargetPod]
}

// represent a Pod identity
type TargetPod struct {
	namespace string
	podName   string
}

// the yaml configuration format
type YamlConfig struct {
	PortForwards []PortForwardOptions `yaml:"portForwards"`
}

func (p TargetPod) String() string {
	return fmt.Sprintf("%s/%s", p.namespace, p.podName)
}

// setting up the cobra cmd
func NewCmd(streams genericiooptions.IOStreams) *cobra.Command {
	c := &Context{
		streams:             streams,
		controllerReadyCh:   make(chan bool, 1),
		portForwardContexts: []*PortForwardContext{},
	}

	configFlags := genericclioptions.NewConfigFlags(true)

	cmd := &cobra.Command{
		Use:          "[kubectl] sticky-port-forward [flags] [port:port]",
		Short:        "doing port forward on a target",
		Example:      fmt.Sprintf(example, "kubectl", "sticky-port-forward"),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {

			c.handleCommand(cmd, configFlags, args)

			c.startMainProcess(configFlags)

			return nil
		},
	}

	cmd.Flags().StringP("file", "f", "", "Use YAML config file to start port forwarding")
	cmd.Flags().StringP("append", "a", "", "Append to a YAML config file instead of port forwarding")
	cmd.Flags().StringP("override", "o", "", "Save to a YAML config file, overriding any existing content, instead of port forwarding")
	cmd.Flags().StringP("pod", "p", "", "Pod name regex")
	cmd.Flags().StringP("labels", "l", "", "Label selectors")
	configFlags.AddFlags(cmd.Flags())

	return cmd
}

// parsing provided command flags and args to build the context
func (c *Context) handleCommand(cmd *cobra.Command, configFlags *genericclioptions.ConfigFlags, args []string) {
	appending, _ := cmd.Flags().GetString("append")
	overriding, _ := cmd.Flags().GetString("override")

	file, _ := cmd.Flags().GetString("file")

	if file == "" && len(args) == 0 {
		// not enough argument provided
		cmd.Help()
		os.Exit(0)
	}

	if appending != "" {
		cfg, err := readConfig(appending)

		if err != nil {
			cfg = &YamlConfig{
				PortForwards: []PortForwardOptions{},
			}
		}

		cfg.PortForwards = append(cfg.PortForwards, parseCommandForOptions(cmd, configFlags, args))

		saveConfig(appending, cfg)

		fmt.Printf("Appended to config file %s successfully\n", appending)

		os.Exit(0)
	} else if overriding != "" {
		yamlConfig := YamlConfig{
			PortForwards: []PortForwardOptions{parseCommandForOptions(cmd, configFlags, args)},
		}

		saveConfig(overriding, &yamlConfig)

		fmt.Printf("Saved config to file %s successfully\n", overriding)

		os.Exit(0)
	} else {
		if file != "" {
			cfg, err := readConfig(file)

			if err != nil {
				panic(err)
			}

			for _, options := range cfg.PortForwards {
				c.portForwardContexts = append(c.portForwardContexts, &PortForwardContext{
					options: options,

					targetPodCh: make(chan *TargetPod),
					runningPod:  atomic.Pointer[TargetPod]{},
				})
			}
		} else {
			c.portForwardContexts = append(c.portForwardContexts, &PortForwardContext{
				options: parseCommandForOptions(cmd, configFlags, args),

				targetPodCh: make(chan *TargetPod),
				runningPod:  atomic.Pointer[TargetPod]{},
			})
		}
	}
}

func parseCommandForOptions(cmd *cobra.Command, configFlags *genericclioptions.ConfigFlags, args []string) PortForwardOptions {
	err := verifyPorts(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	labelParam, _ := cmd.Flags().GetString("labels")
	podSelector, _ := cmd.Flags().GetString("pod")

	labels := map[string]string{}

	if labelParam != "" {
		for _, labelSelectorStr := range strings.Split(labelParam, ",") {
			keyValueSplit := strings.Split(labelSelectorStr, "=")
			labels[keyValueSplit[0]] = keyValueSplit[1]
		}
	}

	return PortForwardOptions{
		NamespaceSelector: *configFlags.Namespace,
		PodSelector:       podSelector,
		LabelSelector:     labels,

		Ports: args,
	}
}

// verify ports input
func verifyPorts(ports []string) error {
	for _, portString := range ports {
		parts := strings.Split(portString, ":")
		var localString, remoteString string
		if len(parts) == 1 {
			localString = parts[0]
			remoteString = parts[0]
		} else if len(parts) == 2 {
			localString = parts[0]
			if localString == "" {
				// support :5000
				localString = "0"
			}
			remoteString = parts[1]
		} else {
			return fmt.Errorf("invalid port format '%s'", portString)
		}

		_, err := strconv.ParseUint(localString, 10, 16)
		if err != nil {
			return fmt.Errorf("error parsing local port '%s': %s", localString, err)
		}

		remotePort, err := strconv.ParseUint(remoteString, 10, 16)
		if err != nil {
			return fmt.Errorf("error parsing remote port '%s': %s", remoteString, err)
		}
		if remotePort == 0 {
			return fmt.Errorf("remote port must be > 0")
		}
	}

	return nil
}

// start the controller & port forwawrding
func (c *Context) startMainProcess(configFlags *genericclioptions.ConfigFlags) {
	c.setupClient(configFlags)

	c.initController()

	fmt.Println("Waiting for controller")
	<-c.controllerReadyCh
	fmt.Println("Controller is ready")

	for _, option := range c.portForwardContexts {
		go c.startForwarding(option)
	}

	// handle terminating application
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	defer signal.Stop(signals)

	<-signals

	close(c.stopControllerCh)

	for _, option := range c.portForwardContexts {
		curRunningPod := option.runningPod.Load()
		if curRunningPod == nil {
			// already closed
			continue
		}
		close(option.stopForwardCh)
	}
}

// setup some client values on the context
func (c *Context) setupClient(configFlags *genericclioptions.ConfigFlags) {
	c.clientConfig, _ = configFlags.ToRawKubeConfigLoader().ClientConfig()
	c.clientset, _ = kubernetes.NewForConfig(c.clientConfig)
}

// standard k8s controller similar to the official example
func (c *Context) initController() {
	podListWatcher := cache.NewListWatchFromClient(c.clientset.CoreV1().RESTClient(), "pods", coreV1.NamespaceAll, fields.Everything())
	c.podQueue = workqueue.NewDelayingQueue()

	indexer, informer := cache.NewIndexerInformer(podListWatcher, &coreV1.Pod{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				c.podQueue.Add(key)
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				c.podQueue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				c.podQueue.Add(key)
			}
		},
	}, cache.Indexers{})

	c.podIndexer = indexer

	go func() {

		stopCh := make(chan struct{}, 1)

		defer runtime.HandleCrash()

		defer c.podQueue.ShutDown()

		go informer.Run(stopCh)

		if !cache.WaitForCacheSync(stopCh, informer.HasSynced) {
			runtime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
			return
		}

		c.controllerReadyCh <- true

		go wait.Until(func() {
			for c.processNextItem() {
			}
		}, time.Second, stopCh)

		c.stopControllerCh = stopCh

		<-stopCh
		klog.Info("Stopping Pod controller")
	}()
}

// processing next k8s controller queue item
func (c *Context) processNextItem() bool {
	key, quit := c.podQueue.Get()
	if quit {
		return false
	}

	defer c.podQueue.Done(key)

	obj, exists, err := c.podIndexer.GetByKey(key.(string))

	if err != nil {
		fmt.Println("Error when processing controller queue", err)
		return true
	}

	keySplit := strings.Split(key.(string), "/")
	targetPod := &TargetPod{
		namespace: keySplit[0],
		podName:   keySplit[1],
	}

	if !exists {
		for _, option := range c.portForwardContexts {
			curRunningPod := option.runningPod.Load()
			if curRunningPod == nil {
				// there is no port-forwarding going on
				continue
			}

			if curRunningPod.namespace == targetPod.namespace && curRunningPod.podName == targetPod.podName {
				fmt.Println("Closing port-forward because pod got deleted", targetPod)
				close(option.stopForwardCh)
			}
		}
	} else {
		pod := obj.(*coreV1.Pod)

		for _, context := range c.portForwardContexts {
			if context.runningPod.Load() != nil {
				// skipped because it's already forwarding
				continue
			}

			if context.isPodMatched(pod) {
				// sending new pod to forward when it's in waiting-for-pod state
				select {
				case context.targetPodCh <- targetPod:
					fmt.Printf("New pod matched condition %s => %s\n", context.options, targetPod)
				default:
					continue
				}
			}
		}
	}

	return true
}

func (c *Context) startForwarding(pfContext *PortForwardContext) {
	for {
		targetPod := c.selectPod(pfContext)

		if targetPod == nil {
			fmt.Printf("No existing pod found for %s, waiting for new pod\n", pfContext.options)
			targetPod = <-pfContext.targetPodCh
		}

		err := c.forwardPod(pfContext, targetPod)
		if err != nil && strings.Contains(err.Error(), "unable to listen on any of the requested ports") {
			panic("Can not start port-forwarding with occupied ports")
		}
		fmt.Println("Stopped port-forwarding", targetPod, err)
	}
}

// try to select an existing pod that matched the provided options
func (c *Context) selectPod(pfContext *PortForwardContext) *TargetPod {
	for _, podObj := range c.podIndexer.List() {
		pod := podObj.(*coreV1.Pod)

		if !pfContext.isPodMatched(pod) {
			continue
		}

		return &TargetPod{
			namespace: pod.ObjectMeta.Namespace,
			podName:   pod.ObjectMeta.Name,
		}
	}

	return nil
}

// check if a pod matched the option
func (pfContext *PortForwardContext) isPodMatched(pod *coreV1.Pod) bool {
	// check for pod's running state
	if pod.Status.Phase != coreV1.PodRunning {
		return false
	}

	// check if there is any container ready
	readiedContainers := 0
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if containerStatus.Ready {
			readiedContainers++
		}
	}
	if readiedContainers == 0 {
		return false
	}

	// matching for the rest of the defined pod options
	if pfContext.options.NamespaceSelector != "" {
		if pod.ObjectMeta.Namespace != pfContext.options.NamespaceSelector {
			return false
		}
	}

	if pfContext.options.PodSelector != "" {
		match, _ := regexp.MatchString(pfContext.options.PodSelector, pod.ObjectMeta.Name)
		if !match {
			return false
		}
	}

	if len(pfContext.options.LabelSelector) > 0 {
		podLabels := pod.ObjectMeta.Labels

		for targetKey, targetVal := range pfContext.options.LabelSelector {
			podVal, ok := podLabels[targetKey]
			if !ok || podVal != targetVal {
				return false
			}
		}
	}

	return true
}

// proceed open a port-forward on a pod
func (c *Context) forwardPod(pfContext *PortForwardContext, targetPod *TargetPod) error {
	req := c.clientset.CoreV1().RESTClient().Post().Resource("pods").
		Namespace(targetPod.namespace).
		Name(targetPod.podName).
		SubResource("portforward")

	fmt.Println("Forwording pod", targetPod)

	transport, upgrader, _ := spdy.RoundTripperFor(c.clientConfig)

	pfContext.stopForwardCh = make(chan struct{}, 1)
	readyChannel := make(chan struct{}, 1)

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, req.URL())

	fw, err := portforward.New(dialer, pfContext.options.Ports, pfContext.stopForwardCh, readyChannel, c.streams.Out, c.streams.ErrOut)
	if err != nil {
		return err
	}

	pfContext.runningPod.Swap(targetPod)
	defer pfContext.runningPod.Swap(nil)

	return fw.ForwardPorts()
}

func readConfig(filepath string) (*YamlConfig, error) {
	data, err := os.ReadFile(filepath)

	if err != nil {
		return nil, err
	}

	// create a person struct and deserialize the data into that struct
	var config YamlConfig

	if err := yaml.Unmarshal(data, &config); err != nil {
		panic(err)
	}

	return &config, nil
}

func saveConfig(filepath string, yamlConfig *YamlConfig) {
	data, err := yaml.Marshal(yamlConfig)

	if err != nil {
		panic(err)
	}

	err = os.WriteFile(filepath, data, 0755)

	if err != nil {
		panic(err)
	}
}
