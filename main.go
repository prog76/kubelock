package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/coordination/v1"
	coordinationv1 "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/transport"
	"k8s.io/klog/v2"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"github.com/bombsimon/logrusr/v4"
)

var (
	args struct {
		cmdName       string
		cmdArgs       []string
		id            string
		kubeconfig    string
		leaseDuration time.Duration
		lockName      string
		lockNamespace string
		renewDeadline time.Duration
		retryPeriod   time.Duration
		lockValue     string
		logLevel      string
	}
	basename = path.Base(os.Args[0])
)

type CustomJSONFormatter struct{}

func (f *CustomJSONFormatter) Format(entry *logrus.Entry) ([]byte, error) {
    data := make(logrus.Fields, len(entry.Data)+4)
    for k, v := range entry.Data {
        switch v := v.(type) {
        case error:
            // Otherwise errors are ignored by `encoding/json`
            // https://github.com/sirupsen/logrus/issues/137
            data[k] = v.Error()
        default:
            data[k] = v
        }
    }

    fileName, lineNumber := f.getFileAndLine()
    timestamp := entry.Time.Format(time.RFC3339)

    data["timestamp"] = timestamp
    data["level"] = strings.ToUpper(entry.Level.String())
    data["msg"] = entry.Message
    data["file"] = fmt.Sprintf("%s:%d", filepath.Base(fileName), lineNumber)

    jsonData, err := json.Marshal(data)
    if err != nil {
        return nil, fmt.Errorf("Failed to marshal fields to JSON, %w", err)
    }

    return append(jsonData, '\n'), nil
}

func (f *CustomJSONFormatter) getFileAndLine() (string, int) {
    var ok bool
    var file string
    var line int

    for i := 0; ; i++ {
        _, file, line, ok = runtime.Caller(i)
        if !ok {
            break
        }
        if !strings.Contains(file, "logrus") && !strings.Contains(file, "log.go") {
            break
        }
    }

    return file, line
}

func init() {
	// The leaderelection library's logging is excessive so we use glog
	// as our logging library and push klog output to /dev/null

	logrus.SetFormatter(&CustomJSONFormatter{})
    logrus.SetOutput(os.Stdout)

	flag.Usage = func() {
		fmt.Printf("Usage: %s [OPTIONS] COMMAND\n\n", basename)
		fmt.Printf("A utility to create/remove locks on Endpoints in Kubernetes while running a shell command/script\n\n")
		fmt.Printf("Options:\n")
		flag.PrintDefaults()
	}
	flag.StringVar(&args.id, "id", os.Getenv("HOSTNAME"), "ID of instance vying for leadership")
	flag.StringVar(&args.kubeconfig, "kubeconfig", "", "Absolute path to the kubeconfig file")
	flag.StringVar(&args.lockName, "name", "example-app", "The lease lock endpoint name")
	flag.StringVar(&args.lockNamespace, "namespace", "default", "The lease lock endpoint namespace")
	flag.DurationVar(&args.leaseDuration, "lease-duration", time.Second*15,
		"How long the lock is held before another client will take over")
	flag.DurationVar(&args.renewDeadline, "renew-deadline", time.Second*10,
		"The duration that the acting master will retry refreshing leadership before giving up.")
	flag.DurationVar(&args.retryPeriod, "retry-period", time.Second*2,
		"The duration clients should wait between attempts to obtain a lock")
	flag.StringVar(&args.lockValue, "lock-value", "", "The value to store in the annotation indicating the current value of the lock")
	flag.StringVar(&args.logLevel, "log-level", "info", "Set the log level [debug|info|warn|error]")
}

// processArgs performs the validation on the command line arguments passed to kubelock
func processArgs(flagArgs []string) error {
	switch len(flagArgs) {
	case 0:
		return errors.New("you must provide a command for kubelock to run")
	case 1:
		args.cmdName = flagArgs[0]
	default:
		args.cmdName = flagArgs[0]
		args.cmdArgs = flagArgs[1:]
	}

	if logrus.GetLevel() >= logrus.DebugLevel {
		logrus.WithFields(logrus.Fields{
			"cmdName":         args.cmdName,
			"cmdArgs":         args.cmdArgs,
			"id":              args.id,
			"leaseDuration":   args.leaseDuration,
			"lockName":        args.lockName,
			"lockNamespace":   args.lockNamespace,
			"renewDeadline":   args.renewDeadline,
			"retryPeriod":     args.retryPeriod,
			"lockValue":       args.lockValue,
		}).Debug("Verbose logging enabled:")
	}
	return nil
}

type KubeConfig struct {
	config        *rest.Config
	clientset     *clientset.Clientset
	lock          *resourcelock.LeaseLock
}

// buildConfig will build a config object for either running outside the cluster using the kubectl config
// (if kubeconfig is set), or in-cluster using the service account assigned to the pods running the binary.
func kubeSetup(kcPath string) (kc *KubeConfig, err error) {
	kc = new(KubeConfig)
	if kcPath != "" {
		kc.config, err = clientcmd.BuildConfigFromFlags("", kcPath)
		if err != nil {
			return nil, err
		}
	} else {
		kc.config, err = rest.InClusterConfig()
		if err != nil {
			return nil, err
		}
	}

	// Create the Clientset - used for querying the various API endpoints
	kc.clientset = clientset.NewForConfigOrDie(kc.config)

	// Create a resourcelock Interface for Endpoints objects since we will be placing the lock
	// annotation on the Endpoints resource assigned to the Service.
	kc.lock = &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      args.lockName,
			Namespace: args.lockNamespace,
		},
		Client: kc.clientset.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: args.id,
		},
	}

	return kc, err
}


const (
	LockAnnotationKey = "kubelock-value"
)
func getEndpointAnnotations(client coordinationv1.LeasesGetter, meta metav1.ObjectMeta, ctx context.Context) (map[string]string, *v1.Lease, error) {
	var lease, err = client.Leases(meta.Namespace).Get(ctx, meta.Name, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}

    annotations := lease.Annotations
    if annotations == nil {
        annotations = make(map[string]string)
    }

    return annotations, lease, err
}

func setEndpointAnnotation(client coordinationv1.LeasesGetter, meta metav1.ObjectMeta, ctx context.Context, value string) error {
	if value == "" {
		return nil
	}
	annotations, endpoints,err := getEndpointAnnotations(client, meta, ctx)
	if err != nil {
		return err
	}
    annotations[LockAnnotationKey] = value
    endpoints.SetAnnotations(annotations)

    _, err = client.Leases(meta.Namespace).Update(ctx, endpoints, metav1.UpdateOptions{})
    return err
}

// hardKill kills the subprocess and any child processes it spawned.
// https://medium.com/@felixge/killing-a-child-process-and-all-of-its-children-in-go-54079af94773
func hardKill(cmd *exec.Cmd) {
	logrus.WithField("action", "hardkill").Info("Proceeding with SIGKILL")
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill() // Fallback
	}
}

// softKill tries to handle a termination signal gently by passing the signal to
// the subprocess and returns a channel with a 90 sec timeout
func softKill(cmd *exec.Cmd, sig os.Signal, timeout time.Duration) (<-chan time.Time, error) {
	logrus.WithField("action", "softkill").Info("Received termination, signaling shutdown")
	if err := cmd.Process.Signal(sig); err != nil {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch, errors.New(fmt.Sprintf("error passing %s sig to subprocess", sig))
	} else {
		return time.After(timeout), nil // Give the subprocess 90s to stop gracefully.
	}
}

func cmdExecutor(cmd *exec.Cmd, spErr *error) {
	// Catch termination signals so they can be passed to the subprocess. Depending
	// on the OS it's probably not possible to catch Kill, but just in case...
	chSignals := make(chan os.Signal, 1)
	signal.Notify(chSignals, os.Interrupt, os.Kill, syscall.SIGTERM)
	defer signal.Stop(chSignals)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	// Create a new process group with the same ID as the PID of the called subprocess. This is so that
	// when we call the hardKill() function we kill any child processes and do not leave any zombies behind.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Start the subprocess
	chSubProcess := make(chan error, 1)
	go func() {
		defer close(chSubProcess)
		*spErr = cmd.Run()
	}()

	var err error
	var chTermTimeout <-chan time.Time
	var softKilled bool
	for {
		select {
		case <-chSubProcess:
			logrus.WithField("status", "subprocess_finished").Debug("chSubProcess: subprocess finished")
			return
		//case <-ctx.Done():
		//	defer hardKill(cmd)
		//	panic("context has been cancelled")
		case s := <-chSignals:
			logrus.WithField("signal_received", s).Debug("kubelock received signal from OS")
			if !softKilled {
				logrus.Debug("attempting soft kill")
				chTermTimeout, err = softKill(cmd, s, 90*time.Second)
				if err != nil {
					logrus.WithError(err).Errorf("Error soft-killing process")
				}
				softKilled = true
			} else {
				logrus.Debug("soft kill already attempted, performing hard kill")
				hardKill(cmd)
			}
		case <-chTermTimeout:
			logrus.Debug("chTermTimeout: received SIGINT, SIGTERM or SIGKILL from OS")
			logrus.Info("Timed out wait for subprocess to exit, killing")
			hardKill(cmd)
		}
	}
}

func getLeaderCallbacks(  ctx context.Context, cancel context.CancelFunc, exec func(), is_done func()(bool)) leaderelection.LeaderCallbacks {
	var leaderCallbacks = leaderelection.LeaderCallbacks{
		OnStartedLeading: func(ctx context.Context) {
			logrus.Info("Lock obtained. Running command.")
			defer cancel() // Exit leaderelection loop and release lock when chSubProcess.
			if(!is_done()) {
				exec()
			}
			time.Sleep(500 * time.Millisecond)
		},
		OnStoppedLeading: func() {
			select {
			case <-ctx.Done():
				logrus.Info("Lock released.")
			default:
				// The lock shouldn't be released until OnStartedLeading finishes
				// and the context is canceled.
				panic("Lock released.")
			}
		},
		OnNewLeader: func(identity string) {
			if identity == args.id {
				return
			}
			logrus.Infof("Current leader is %v", identity)
			if is_done() {
				cancel();
			}
		},
	}
	return leaderCallbacks
}


func main() {

	// Process options/arguments
	flag.Parse()
	if err := processArgs(flag.Args()); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s\n", basename, err)
		flag.Usage()
		os.Exit(1)
	}

    flag.Parse()

    // Convert the log level string to a logrus.Level
    level, err := logrus.ParseLevel(strings.ToLower(args.logLevel))
    if err != nil {
        logrus.Fatalf("Invalid log level: %s\n", err)
    }

    // Set the log level for logrus
    logrus.SetLevel(level)
	defaultLogger := logrus.StandardLogger()
	log := logrusr.New(defaultLogger)
	klog.SetLogger(log)

	// Get the config and lock objects required for the leaderelection
	kc, err := kubeSetup(args.kubeconfig)
	if err != nil {
		logrus.Fatalf("Unable to setup kube config: %v", err)
	}
	// Create a context with a cancel function that we will pass to the locking function
	// (leaderelection.RunOrDie).
	ctx, cancel := context.WithCancel(context.Background())
	// Add a WrapperFunc to the connection config to prevent any more requests
	// being sent to the API server after the above context has been cancelled.
	kc.config.Wrap(transport.ContextCanceller(ctx, fmt.Errorf("the leader is shutting down")))

	// Set up a variable in which the callback can store the output of the cmd.Run()
	// so we can query it for non-zero return codes at the end.
	var subprocessErr error
	is_done :=func() (bool) {
		if args.lockValue == "" {
			return true
		}
		annotations, _, err := getEndpointAnnotations(kc.lock.Client, kc.lock.LeaseMeta, context.TODO())
		if err != nil {
			logrus.Errorf("Error reading annotations: %s", err)
		}
		if annotations[LockAnnotationKey]==args.lockValue {
			logrus.WithFields(logrus.Fields{
				"Namespace":       args.lockNamespace,
				"Lease name":      args.lockName,
				"Lease value":     args.lockValue}).Info("Job is already done")
			return true
		}
		return false
	}
	if(is_done()) {
		os.Exit(0)
	}
	cmdExecuted := false
	cb := getLeaderCallbacks(ctx, cancel,
		func(){
			cmdExecuted = true
			cmdExecutor(exec.CommandContext(ctx, args.cmdName, args.cmdArgs...), &subprocessErr)
		},
		is_done,
	)

	// Start the leaderelection loop
	logrus.Infof("Attempting to get a lock on %s/%v", args.lockNamespace, args.lockName)
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            kc.lock,
		LeaseDuration:   args.leaseDuration,
		RenewDeadline:   args.renewDeadline,
		RetryPeriod:     args.retryPeriod,
		Callbacks:       cb,
		ReleaseOnCancel: true,
	})

	if subprocessErr != nil {
		if e, ok := subprocessErr.(*exec.ExitError); ok {
			os.Exit(e.ExitCode())
		} else {
			logrus.Errorf("Error starting subprocess: %s", subprocessErr.Error())
		}
	}else if cmdExecuted {
		err := setEndpointAnnotation(kc.lock.Client, kc.lock.LeaseMeta, context.TODO(), args.lockValue)
		if err != nil {
			logrus.Errorf("Error updating annotations: %s", err)
		}
	}
}
