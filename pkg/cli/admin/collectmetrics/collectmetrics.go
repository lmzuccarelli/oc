package collectmetrics

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path"
	//"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/klog/v2"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/kubectl/pkg/cmd/logs"
	kcmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util/templates"
	admissionapi "k8s.io/pod-security-admission/api"
	"k8s.io/utils/exec"

	configclient "github.com/openshift/client-go/config/clientset/versioned"
	imagev1client "github.com/openshift/client-go/image/clientset/versioned/typed/image/v1"
	imagereference "github.com/openshift/library-go/pkg/image/reference"
	"github.com/openshift/library-go/pkg/operator/resource/retry"
	rbacv1client "k8s.io/client-go/kubernetes/typed/rbac/v1"

	"github.com/openshift/oc/pkg/cli/admin/inspect"
	policy "github.com/openshift/oc/pkg/cli/admin/policy"
	"github.com/openshift/oc/pkg/cli/rsync"
	ocmdhelpers "github.com/openshift/oc/pkg/helpers/cmd"
)

var (
	collectMetricsLong = templates.LongDesc(`
		Launch a pod to gather debugging information.

		This command will launch a pod in a temporary namespace on your cluster that gathers
		debugging information and then downloads the gathered information.

		Experimental: This command is under active development and may change without notice.
	`)

	collectMetricsExample = templates.Examples(`
		# Collect metrics information using the default plug-in image and command, writing into ./collect-metrics.local.<rand>
		  oc adm collect-metrics

		# Collect metrics information with a specific local folder to copy to
		  oc adm collect-metrics --dest-dir=/local/directory


		# Collect metrics information using a specific image, command, and pod directory
		  oc adm collect-metrics --image=my/image:tag --source-dir=/pod/directory -- myspecial-command.sh
		
			# Collect metrics information using a specific image, command, and pod directory and environment variables
		  oc adm collect-metrics --envar "EXECUTE_SCRIPT-/test.sh" --envar" WORKING_DIR=/tmp/test" --image=my/image:tag --source-dir=/pod/directory -- myspecial-command.sh
	`)
)

func NewCollectMetricsCommand(f kcmdutil.Factory, streams genericclioptions.IOStreams) *cobra.Command {
	o := NewCollectMetricOptions(streams)
	cmd := &cobra.Command{
		Use:     "collect-metrics",
		Short:   "Launch a new instance of a pod for collecting specific metrics",
		Long:    collectMetricsLong,
		Example: collectMetricsExample,
		Run: func(cmd *cobra.Command, args []string) {
			kcmdutil.CheckErr(o.Complete(f, cmd, args))
			kcmdutil.CheckErr(o.Validate())
			ocmdhelpers.CheckPodSecurityErr(o.Run())
		},
	}

	cmd.Flags().StringVar(&o.NodeName, "node-name", o.NodeName, "Set a specific node to use - by default a random master will be used.")
	cmd.Flags().StringVar(&o.NodeSelector, "node-selector", o.NodeSelector, "Set a specific node selector to use - only relevant when specifying a command and image which needs to capture data on a set of cluster nodes simultaneously.")
	cmd.Flags().StringVar(&o.Command, "command", o.Command, "The script to execute in the container.")
	cmd.Flags().StringSliceVar(&o.Args, "cmd-args", o.Args, "Specify arguments (in specific order) for the command (script to execute).")
	cmd.Flags().StringVar(&o.SourceDir, "src-dir", o.SourceDir, "The path to mount for metrics results artifacts (i.e tar.gz)")
	cmd.Flags().StringVar(&o.Image, "image", o.Image, "Specify a collect-metrics plugin image to run. If not specified, OpenShift's default collect-metrics image will be used.")
	cmd.Flags().StringSliceVar(&o.Envars, "envar", o.Envars, "Specify envars using the notation k=v, if notation is incorrect it won't be added to the pods.")
	cmd.Flags().StringVar(&o.DestDir, "dest-dir", o.DestDir, "Set a specific directory on the local machine to write gathered data to.")
	cmd.Flags().StringVar(&o.timeoutStr, "timeout", "10m", "The length of time to gather data, like 5s, 2m, or 3h, higher than zero. Defaults to 10 minutes.")
	cmd.Flags().BoolVar(&o.Keep, "keep", o.Keep, "Do not delete temporary resources when command completes.")
	// nolint: errcheck
	cmd.Flags().MarkHidden("keep")

	return cmd
}

func NewCollectMetricOptions(streams genericclioptions.IOStreams) *CollectMetricOptions {
	return &CollectMetricOptions{
		IOStreams:              streams,
		LogOut:                 newPrefixWriter(streams.Out, "[collect-metrics      ] OUT"),
		RawOut:                 streams.Out,
		Timeout:                10 * time.Minute,
		SCCModificationOptions: policy.NewSCCModificationOptions(streams),
	}
}

func (o *CollectMetricOptions) Complete(f kcmdutil.Factory, cmd *cobra.Command, args []string) error {
	o.RESTClientGetter = f
	var err error
	if o.Config, err = f.ToRESTConfig(); err != nil {
		return err
	}
	if o.Client, err = kubernetes.NewForConfig(o.Config); err != nil {
		return err
	}
	if o.ConfigClient, err = configclient.NewForConfig(o.Config); err != nil {
		return err
	}
	if o.ImageClient, err = imagev1client.NewForConfig(o.Config); err != nil {
		return err
	}

	if len(o.timeoutStr) > 0 {
		if strings.ContainsAny(o.timeoutStr, "smh") {
			o.Timeout, err = time.ParseDuration(o.timeoutStr)
			if err != nil {
				return fmt.Errorf(`invalid argument %q for "--timeout" flag: %v`, o.timeoutStr, err)
			}
		} else {
			fmt.Fprint(o.ErrOut, "Flag --timeout's value in seconds has been deprecated, use duration like 5s, 2m, or 3h, instead\n")
			i, err := strconv.ParseInt(o.timeoutStr, 0, 64)
			if err != nil {
				return fmt.Errorf(`invalid argument %q for "--timeout" flag: %v`, o.timeoutStr, err)
			}
			o.Timeout = time.Duration(i) * time.Second
		}
	}
	if len(o.DestDir) == 0 {
		o.DestDir = fmt.Sprintf("collect-metrics.local.%06d", rand.Int63())
	}
	if err := o.completeImages(); err != nil {
		return err
	}
	o.PrinterCreated, err = printers.NewTypeSetter(scheme.Scheme).WrapToPrinter(&printers.NamePrinter{Operation: "created"}, nil)
	if err != nil {
		return err
	}
	o.PrinterDeleted, err = printers.NewTypeSetter(scheme.Scheme).WrapToPrinter(&printers.NamePrinter{Operation: "deleted"}, nil)
	if err != nil {
		return err
	}
	o.RsyncRshCmd = rsync.DefaultRsyncRemoteShellToUse(cmd)
	// setup sa to add to scc
	o.SCCModificationOptions.SCCName = "privileged"
	sa := rbacv1.Subject{
		Kind:      "ServiceAccount",
		Name:      "default",
		Namespace: "openshift-collect-metrics",
	}
	o.SCCModificationOptions.Subjects = []rbacv1.Subject{sa}
	o.SCCModificationOptions.ToPrinter = func(operation string) (printers.ResourcePrinter, error) {
		return o.SCCModificationOptions.PrintFlags.ToPrinter()
	}
	o.SCCModificationOptions.RbacClient, err = rbacv1client.NewForConfig(o.Config)
	if err != nil {
		return err
	}
	o.HostNetwork = true
	return nil
}

func (o *CollectMetricOptions) completeImages() error {
	if len(o.Image) == 0 {
		o.Image = "quay.io/node-observability-operator/node-observability-agent:latest"
	}
	o.log("Using collect-metrics plug-in image: %s", o.Image)
	return nil
}

type CollectMetricOptions struct {
	genericclioptions.IOStreams

	Config           *rest.Config
	Client           kubernetes.Interface
	ConfigClient     configclient.Interface
	ImageClient      imagev1client.ImageV1Interface
	RESTClientGetter genericclioptions.RESTClientGetter

	NodeName     string
	NodeSelector string
	HostNetwork  bool
	Scripting    bool
	DestDir      string
	SourceDir    string
	Image        string
	Envars       []string
	Command      string
	Args         []string
	Timeout      time.Duration
	timeoutStr   string
	RunNamespace string
	Keep         bool

	RsyncRshCmd string

	PrinterCreated printers.ResourcePrinter
	PrinterDeleted printers.ResourcePrinter
	LogOut         io.Writer
	// RawOut is used for printing information we're looking to have copy/pasted into bugs
	RawOut io.Writer

	SCCModificationOptions *policy.SCCModificationOptions
}

func (o *CollectMetricOptions) Validate() error {
	if o.Command == "" {
		return fmt.Errorf("--command is required, this is the name of the script to execute")
	}
	if o.SourceDir == "" {
		return fmt.Errorf("--src-dir is required, this is the directory to mount to copy artifacts from")
	}
	if o.NodeName != "" && o.NodeSelector != "" {
		return fmt.Errorf("--node-name and --node-selector are mutually exclusive: please specify one or the other")
	}
	// validation from vendor/k8s.io/apimachinery/pkg/api/validation/path/name.go
	if len(o.NodeName) > 0 && strings.ContainsAny(o.NodeName, "/%") {
		return fmt.Errorf("--node-name may not contain '/' or '%%'")
	}
	if strings.Contains(o.DestDir, ":") {
		return fmt.Errorf("--dest-dir may not contain special characters such as colon(:)")
	}
	return nil
}

// Run creates and runs a collect-metrics pod.d
func (o *CollectMetricOptions) Run() error {
	var errs []error

	klog.Info("executing script ", o.Command)
	klog.Info("arguments ", o.Args)
	// print at both the beginning and at the end.  This information is important enough to be in both spots.
	o.PrintBasicClusterState(context.TODO())
	defer func() {
		fmt.Fprintf(o.RawOut, "\n\n")
		fmt.Fprintf(o.RawOut, "Reprinting Cluster State:\n")
		o.PrintBasicClusterState(context.TODO())
	}()

	err := o.SCCModificationOptions.AddSCC()
	if err != nil {
		fmt.Println("ERROR with scc ", err)
	}

	// Get or create "working" namespace ...
	ns, _, err := o.getNamespace()
	if err != nil {
		// ensure the errors bubble up to BackupCollect metricsing method for display
		// errs = []error{err}
		return err
	}

	// ... ensure resource cleanup unless instructed otherwise ...
	if !o.Keep {
		defer func() {
			if err := o.Client.CoreV1().Namespaces().Delete(context.TODO(), ns.Name, metav1.DeleteOptions{}); err != nil {
				fmt.Printf("%v\n", err)
			} else {
				//nolint: errcheck
				o.PrinterDeleted.PrintObj(ns, o.LogOut)
			}
		}()
	}

	// Prefer to run in master if there's any but don't be explicit otherwise.
	// This enables the command to run by default in hypershift where there's no masters.
	nodes, err := o.Client.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{
		LabelSelector: o.NodeSelector,
	})
	if err != nil {
		return err
	}
	var hasMaster bool
	for _, node := range nodes.Items {
		if _, ok := node.Labels["node-role.kubernetes.io/master"]; ok {
			hasMaster = true
			break
		}
	}

	// ... and create collect-metrics pod(s)
	var pods []*corev1.Pod
	var podSchema *corev1.Pod

	img, err := imagereference.Parse(o.Image)
	if err != nil {
		line := fmt.Sprintf("unable to parse image reference %s: %v", img, err)
		o.log(line)
		return err
	}
	if o.NodeSelector != "" {
		nodes, err := o.Client.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{
			LabelSelector: o.NodeSelector,
		})
		if err != nil {
			return err
		}
		for x, node := range nodes.Items {
			podSchema = o.newScriptingPod(node.Name, x, o.Image, o.Envars, hasMaster)
			pod, err := o.Client.CoreV1().Pods(ns.Name).Create(context.TODO(), podSchema, metav1.CreateOptions{})
			if err != nil {
				return err
			}
			o.log("pod: %s on node: %s for plug-in image %s created", pod.Name, node.Name, o.Image)
			pods = append(pods, pod)
		}
	} else {
		if o.NodeName != "" {
			if _, err := o.Client.CoreV1().Nodes().Get(context.TODO(), o.NodeName, metav1.GetOptions{}); err != nil {
				return err
			}
		}
		podSchema = o.newScriptingPod(o.NodeName, 0, o.Image, o.Envars, hasMaster)
		pod, err := o.Client.CoreV1().Pods(ns.Name).Create(context.TODO(), podSchema, metav1.CreateOptions{})
		if err != nil {
			return err
		}
		o.log("pod for plug-in image %s created", o.Image)
		pods = append(pods, pod)
	}

	// log timestamps...
	if err := os.MkdirAll(o.DestDir, os.ModePerm); err != nil {
		return err
	}
	if err := o.logTimestamp(); err != nil {
		return err
	}
	// nolint: errcheck
	defer o.logTimestamp()

	var wg sync.WaitGroup
	wg.Add(len(pods))
	errCh := make(chan error, len(pods))
	for _, pod := range pods {
		go func(pod *corev1.Pod) {
			defer wg.Done()

			if len(o.RunNamespace) > 0 && !o.Keep {
				defer func() {
					// collect-metrics runs in its own separate namespace as default , so after it is completed
					// it deletes this namespace and all the pods are removed by garbage collector.
					// However, if user specifies namespace via `run-namespace`, these pods need to
					// be deleted manually.
					err = o.Client.CoreV1().Pods(o.RunNamespace).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})
					if err != nil {
						klog.V(4).Infof("pod deletion error %v", err)
					}
				}()
			}

			log := newPodOutLogger(o.Out, pod.Name)
			// wait for collect-metrics container to be running
			if err := o.waitForCollectMetricsContainerRunning(pod); err != nil {
				log("collect-metrics did not start: %s", err)
				errCh <- fmt.Errorf("collect-metrics did not start for pod %s: %s", pod.Name, err)
				return
			}
			// stream collect-metrics container logs
			if err := o.getCollectMetricsContainerLogs(pod); err != nil {
				log("collect-metrics logs unavailable: %v", err)
			}

			// wait for pod to be running (collect-metrics has completed)
			log("waiting for collect-metrics to complete")
			if err := o.waitForCollectMetricsToComplete(pod); err != nil {
				log("collect-metrics never finished: %v", err)
				if exiterr, ok := err.(*exec.CodeExitError); ok {
					errCh <- exiterr
				} else {
					errCh <- fmt.Errorf("collect-metrics never finished for pod %s: %s", pod.Name, err)
				}
				return
			}

			// copy the gathered files into the local destination dir
			log("downloading collect-metrics output")
			pod, err = o.Client.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{})
			if err != nil {
				log("collect-metrics output not downloaded: %v\n", err)
				errCh <- fmt.Errorf("unable to download output from pod %s: %s", pod.Name, err)
				return
			}
			if err := o.copyFilesFromPod(pod); err != nil {
				log("collect-metrics output not downloaded: %v\n", err)
				errCh <- fmt.Errorf("unable to download output from pod %s: %s", pod.Name, err)
				return
			}
		}(pod)
	}
	wg.Wait()
	close(errCh)

	for i := range errCh {
		errs = append(errs, i)
	}

	// now gather all the events into a single file and produce a unified file
	if err := inspect.CreateEventFilterPage(o.DestDir); err != nil {
		errs = append(errs, err)
	}

	return errors.NewAggregate(errs)
}

func newPodOutLogger(out io.Writer, podName string) func(string, ...interface{}) {
	writer := newPrefixWriter(out, fmt.Sprintf("[%s] OUT", podName))
	return func(format string, a ...interface{}) {
		fmt.Fprintf(writer, format+"\n", a...)
	}
}

func (o *CollectMetricOptions) log(format string, a ...interface{}) {
	fmt.Fprintf(o.LogOut, format+"\n", a...)
}

func (o *CollectMetricOptions) logTimestamp() error {
	f, err := os.OpenFile(path.Join(o.DestDir, "timestamp"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(fmt.Sprintf("%v\n", time.Now()))
	return err
}

func (o *CollectMetricOptions) copyFilesFromPod(pod *corev1.Pod) error {
	streams := o.IOStreams
	streams.Out = newPrefixWriter(streams.Out, fmt.Sprintf("[%s] OUT", pod.Name))
	imageFolder := pod.Spec.NodeName
	destDir := path.Join(o.DestDir, imageFolder)
	if err := os.MkdirAll(destDir, os.ModePerm); err != nil {
		return err
	}
	rsyncOptions := &rsync.RsyncOptions{
		Namespace:     pod.Namespace,
		Source:        &rsync.PathSpec{PodName: pod.Name, Path: path.Clean(o.SourceDir) + "/"},
		ContainerName: "copy",
		Destination:   &rsync.PathSpec{PodName: "", Path: destDir},
		Client:        o.Client,
		Config:        o.Config,
		Compress:      true,
		RshCmd:        fmt.Sprintf("%s --namespace=%s -c copy", o.RsyncRshCmd, pod.Namespace),
		IOStreams:     streams,
	}
	rsyncOptions.Strategy = rsync.NewDefaultCopyStrategy(rsyncOptions)
	err := rsyncOptions.RunRsync()
	if err != nil {
		klog.V(4).Infof("re-trying rsync after initial failure %v", err)
		// re-try copying data before letting it go
		err = rsyncOptions.RunRsync()
	}
	return err
}

func (o *CollectMetricOptions) getCollectMetricsContainerLogs(pod *corev1.Pod) error {
	since2s := int64(2)
	opts := &logs.LogsOptions{
		Namespace:   pod.Namespace,
		ResourceArg: pod.Name,
		Options: &corev1.PodLogOptions{
			Follow:     true,
			Container:  pod.Spec.Containers[0].Name,
			Timestamps: true,
		},
		RESTClientGetter: o.RESTClientGetter,
		Object:           pod,
		ConsumeRequestFn: logs.DefaultConsumeRequest,
		LogsForObject:    polymorphichelpers.LogsForObjectFn,
		IOStreams:        genericclioptions.IOStreams{Out: newPrefixWriter(o.Out, fmt.Sprintf("[%s] POD", pod.Name))},
	}

	for {
		// gather script might take longer than the default API server time,
		// so we should check if the gather script still runs and re-run logs
		// thus we run this in a loop
		if err := opts.RunLogs(); err != nil {
			return err
		}

		// to ensure we don't print all of history set since to past 2 seconds
		opts.Options.(*corev1.PodLogOptions).SinceSeconds = &since2s
		if done, _ := o.isCollectMetricsDone(pod); done {
			return nil
		}
		klog.V(4).Infof("lost logs, re-trying...")
	}
}

func newPrefixWriter(out io.Writer, prefix string) io.Writer {
	reader, writer := io.Pipe()
	scanner := bufio.NewScanner(reader)
	go func() {
		for scanner.Scan() {
			fmt.Fprintf(out, "%s %s\n", prefix, scanner.Text())
		}
	}()
	return writer
}

func (o *CollectMetricOptions) waitForCollectMetricsToComplete(pod *corev1.Pod) error {
	return wait.PollImmediate(10*time.Second, o.Timeout, func() (bool, error) {
		return o.isCollectMetricsDone(pod)
	})
}

func (o *CollectMetricOptions) isCollectMetricsDone(pod *corev1.Pod) (bool, error) {
	var err error
	if pod, err = o.Client.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{}); err != nil {
		// at this stage pod should exist, we've been gathering container logs, so error if not found
		if kerrors.IsNotFound(err) {
			return true, err
		}
		return false, nil
	}
	var state *corev1.ContainerState
	for _, cstate := range pod.Status.ContainerStatuses {
		if cstate.Name == "scripting" {
			state = &cstate.State
			break
		}
	}

	// missing status for gather container => timeout in the worst case
	if state == nil {
		return false, nil
	}

	if state.Terminated != nil {
		if state.Terminated.ExitCode == 0 {
			return true, nil
		}
		return true, &exec.CodeExitError{
			Err:  fmt.Errorf("%s/%s unexpectedly terminated: exit code: %v, reason: %s, message: %s", pod.Namespace, pod.Name, state.Terminated.ExitCode, state.Terminated.Reason, state.Terminated.Message),
			Code: int(state.Terminated.ExitCode),
		}
	}
	return false, nil
}

func (o *CollectMetricOptions) waitForCollectMetricsContainerRunning(pod *corev1.Pod) error {
	return wait.PollImmediate(10*time.Second, o.Timeout, func() (bool, error) {
		var err error
		if pod, err = o.Client.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{}); err == nil {
			if len(pod.Status.ContainerStatuses) == 0 {
				return false, nil
			}
			state := pod.Status.ContainerStatuses[0].State
			if state.Waiting != nil {
				switch state.Waiting.Reason {
				case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
					return true, fmt.Errorf("unable to pull image: %v: %v", state.Waiting.Reason, state.Waiting.Message)
				}
			}
			running := state.Running != nil
			terminated := state.Terminated != nil
			return running || terminated, nil
		}
		if retry.IsHTTPClientError(err) {
			return false, nil
		}
		return false, err
	})
}

func (o *CollectMetricOptions) getNamespace() (*corev1.Namespace, func(), error) {
	if o.RunNamespace == "" {
		return o.createTempNamespace()
	}

	ns, err := o.Client.CoreV1().Namespaces().Get(context.TODO(), o.RunNamespace, metav1.GetOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("retrieving namespace %q: %w", o.RunNamespace, err)
	}

	return ns, func() {}, nil
}

func (o *CollectMetricOptions) createTempNamespace() (*corev1.Namespace, func(), error) {
	ns, err := o.Client.CoreV1().Namespaces().Create(context.TODO(), newNamespace(), metav1.CreateOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("creating temp namespace: %w", err)
	}
	// nolint: errcheck
	o.PrinterCreated.PrintObj(ns, o.LogOut)

	cleanup := func() {
		if err := o.Client.CoreV1().Namespaces().Delete(context.TODO(), ns.Name, metav1.DeleteOptions{}); err != nil {
			fmt.Printf("%v\n", err)
		} else {
			//nolint: errcheck
			o.PrinterDeleted.PrintObj(ns, o.LogOut)
		}
	}

	return ns, cleanup, nil
}

func newNamespace() *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "openshift-collect-metrics",
			Labels: map[string]string{
				"openshift.io/run-level":                         "0",
				admissionapi.EnforceLevelLabel:                   string(admissionapi.LevelPrivileged),
				admissionapi.AuditLevelLabel:                     string(admissionapi.LevelPrivileged),
				admissionapi.WarnLevelLabel:                      string(admissionapi.LevelPrivileged),
				"security.openshift.io/scc.podSecurityLabelSync": "false",
			},
			Annotations: map[string]string{
				"oc.openshift.io/command":    "oc adm collect-metrics",
				"openshift.io/node-selector": "",
			},
		},
	}
}

// newScriptingPod creates a pod with embedded metrics scripts
func (o *CollectMetricOptions) newScriptingPod(node string, podnumber int, image string, envars []string, hasMaster bool) *corev1.Pod {
	zero := int64(0)
	var envs []corev1.EnvVar

	envs = []corev1.EnvVar{
		{
			Name: "NODE_NAME",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "spec.nodeName",
				},
			},
		},
		{
			Name: "POD_NAME",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.name",
				},
			},
		},
	}

	for _, env := range envars {
		kv := strings.Split(env, "=")
		if len(kv) > 0 {
			sch := corev1.EnvVar{
				Name:  kv[0],
				Value: kv[1],
			}
			envs = append(envs, sch)
		}
	}

	nodeSelector := map[string]string{
		corev1.LabelOSStable: "linux",
	}
	if node == "" && hasMaster {
		nodeSelector["node-role.kubernetes.io/master"] = ""
	}

	ret := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "collect-metrics-scripting-" + strconv.Itoa(podnumber),
			Labels: map[string]string{
				"app": "collect-metrics",
			},
		},
		Spec: corev1.PodSpec{
			NodeName: node,
			// This pod is ok to be OOMKilled but not preempted. Following the conventions mentioned at:
			// https://github.com/openshift/enhancements/blob/master/CONVENTIONS.md#priority-classes
			// so setting priority class to system-cluster-critical
			PriorityClassName:  "system-cluster-critical",
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: "default",
			Volumes: []corev1.Volume{
				{
					Name: "collect-metrics-output",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:            "scripting",
					Image:           image,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command:         []string{o.Command},
					Args:            o.Args,
					Env:             envs,
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "collect-metrics-output",
							MountPath: path.Clean(o.SourceDir),
							ReadOnly:  false,
						},
					},
				},
				{
					Name:            "copy",
					Image:           "registry.access.redhat.com/ubi9/ubi-init:latest",
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command:         []string{"/bin/bash", "-c", "trap : TERM INT; sleep infinity & wait"},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "collect-metrics-output",
							MountPath: path.Clean(o.SourceDir),
							ReadOnly:  false,
						},
					},
				},
			},
			HostNetwork:                   o.HostNetwork,
			NodeSelector:                  nodeSelector,
			TerminationGracePeriodSeconds: &zero,
			Tolerations: []corev1.Toleration{
				{
					// An empty key with operator Exists matches all keys,
					// values and effects which means this will tolerate everything.
					// As noted in https://kubernetes.io/docs/concepts/scheduling-eviction/taint-and-toleration/
					Operator: "Exists",
				},
			},
		},
	}

	priv := true
	// If a user specified hostNetwork he might have intended to perform
	// packet captures on the host, for that we need to set the correct
	// capability. Limit the capability to CAP_NET_RAW though, as that's the
	// only capability which does not allow for more than what can be
	// considered "reading"
	ret.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{
		Capabilities: &corev1.Capabilities{
			Add: []corev1.Capability{
				corev1.Capability("CAP_NET_RAW"),
				corev1.Capability("NET_ADMIN"),
			},
		},
		Privileged: &priv,
	}
	return ret
}
