package service

import (
	"bufio"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/credentialprovider"
	_ "github.com/GoogleCloudPlatform/kubernetes/pkg/healthz"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/kubelet"
	kconfig "github.com/GoogleCloudPlatform/kubernetes/pkg/kubelet/config"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/kubelet/dockertools"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/kubelet/server"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	log "github.com/golang/glog"
	bindings "github.com/mesos/mesos-go/executor"
	"github.com/mesosphere/kubernetes-mesos/pkg/executor"

	"github.com/spf13/pflag"
)

const (
	MESOS_CFG_SOURCE = "mesos" // @see ConfigSourceAnnotationKey
)

type KubeletExecutorServer struct {
	*server.KubeletServer
	RunProxy     bool
	ProxyLogV    int
	ProxyExec    string
	ProxyLogfile string
	ProxyBindall bool
}

func NewKubeletExecutorServer() *KubeletExecutorServer {
	k := &KubeletExecutorServer{
		KubeletServer: server.NewKubeletServer(),
	}
	if pwd, err := os.Getwd(); err != nil {
		log.Warningf("failed to determine current directory: %v", err)
	} else {
		k.RootDirectory = pwd // mesos sandbox dir
	}
	k.Address = util.IP(net.ParseIP(defaultBindingAddress()))
	return k
}

func (s *KubeletExecutorServer) AddFlags(fs *pflag.FlagSet) {
	s.KubeletServer.AddFlags(fs)
	fs.BoolVar(&s.RunProxy, "run_proxy", true, "Maintain a running kube-proxy instance as a child proc of this kubelet-executor.")
	fs.IntVar(&s.ProxyLogV, "proxy_logv", 0, "Log verbosity of the child kube-proxy.")
	fs.StringVar(&s.ProxyExec, "proxy_exec", "./kube-proxy", "Path to the kube-proxy executable.")
	fs.StringVar(&s.ProxyLogfile, "proxy_logfile", "./proxy-log", "Path to the kube-proxy log file.")
	fs.BoolVar(&s.ProxyBindall, "proxy_bindall", false, "When true will cause kube-proxy to bind to 0.0.0.0. Defaults to false.")
}

// Run runs the specified KubeletExecutorServer.
func (s *KubeletExecutorServer) Run(_ []string) error {
	util.ReallyCrash = s.ReallyCrashForTesting
	rand.Seed(time.Now().UTC().UnixNano())

	if err := util.ApplyOomScoreAdj(s.OOMScoreAdj); err != nil {
		log.Info(err)
	}

	client, err := s.CreateAPIServerClient()
	if err != nil && len(s.APIServerList) > 0 {
		// required for k8sm since we need to send api.Binding information
		// back to the apiserver
		log.Fatalf("No API client: %v", err)
	}

	credentialprovider.SetPreferredDockercfgPath(s.RootDirectory)

	kcfg := server.KubeletConfig{
		Address:                 s.Address,
		AllowPrivileged:         s.AllowPrivileged,
		HostnameOverride:        s.HostnameOverride,
		RootDirectory:           s.RootDirectory,
		FileCheckFrequency:      s.FileCheckFrequency,
		HTTPCheckFrequency:      s.HTTPCheckFrequency,
		PodInfraContainerImage:  s.PodInfraContainerImage,
		SyncFrequency:           s.SyncFrequency,
		RegistryPullQPS:         s.RegistryPullQPS,
		RegistryBurst:           s.RegistryBurst,
		MinimumGCAge:            s.MinimumGCAge,
		MaxContainerCount:       s.MaxContainerCount,
		ClusterDomain:           s.ClusterDomain,
		ClusterDNS:              s.ClusterDNS,
		Port:                    s.Port,
		CAdvisorPort:            s.CAdvisorPort,
		EnableServer:            s.EnableServer,
		EnableDebuggingHandlers: s.EnableDebuggingHandlers,
		DockerClient:            dockertools.ConnectToDockerOrDie(s.DockerEndpoint),
		KubeClient:              client,
		EtcdClient:              kubelet.EtcdClientOrDie(util.StringList{}, ""), // this kubelet doesn't use etcd
		MasterServiceNamespace:  s.MasterServiceNamespace,
		VolumePlugins:           server.ProbeVolumePlugins(),
	}

	finished := make(chan struct{})
	server.RunKubelet(&kcfg, server.KubeletBuilder(func(kc *server.KubeletConfig) (server.KubeletBootstrap, *kconfig.PodConfig, error) {
		return s.createAndInitKubelet(kc, finished)
	}))

	// block until driver is shut down
	select {
	case <-finished:
	}
	log.Infoln("kubelet executor exiting")
	return nil
}

func defaultBindingAddress() string {
	libProcessIP := os.Getenv("LIBPROCESS_IP")
	if libProcessIP == "" {
		return "0.0.0.0"
	} else {
		return libProcessIP
	}
}

func (ks *KubeletExecutorServer) createAndInitKubelet(kc *server.KubeletConfig, finished chan struct{}) (server.KubeletBootstrap, *kconfig.PodConfig, error) {
	watch := kubelet.SetupEventSending(kc.KubeClient, kc.Hostname)
	pc := kconfig.NewPodConfig(kconfig.PodConfigNotificationSnapshotAndUpdates)
	updates := pc.Channel(MESOS_CFG_SOURCE)

	// TODO(k8s): block until all sources have delivered at least one update to the channel, or break the sync loop
	// up into "per source" synchronizations

	kubelet, err := kubelet.NewMainKubelet(
		kc.Hostname,
		kc.DockerClient,
		kc.EtcdClient,
		kc.KubeClient,
		kc.RootDirectory,
		kc.PodInfraContainerImage,
		kc.SyncFrequency,
		float32(kc.RegistryPullQPS),
		kc.RegistryBurst,
		kc.MinimumGCAge,
		kc.MaxContainerCount,
		pc.IsSourceSeen,
		kc.ClusterDomain,
		net.IP(kc.ClusterDNS),
		kc.MasterServiceNamespace,
		kc.VolumePlugins)
	if err != nil {
		return nil, nil, err
	}
	k := &kubeletExecutor{
		Kubelet:        kubelet,
		finished:       finished,
		runProxy:       ks.RunProxy,
		proxyLogV:      ks.ProxyLogV,
		proxyExec:      ks.ProxyExec,
		proxyLogfile:   ks.ProxyLogfile,
		proxyBindall:   ks.ProxyBindall,
		address:        ks.Address,
		etcdServerList: ks.EtcdServerList,
		etcdConfigFile: ks.EtcdConfigFile,
	}

	exec := executor.New(k.Kubelet, updates, MESOS_CFG_SOURCE, kc.KubeClient, watch, kc.DockerClient)
	dconfig := bindings.DriverConfig{
		Executor:         exec,
		HostnameOverride: ks.HostnameOverride,
		BindingAddress:   net.IP(ks.Address),
	}
	if driver, err := bindings.NewMesosExecutorDriver(dconfig); err != nil {
		log.Fatalf("failed to create executor driver: %v", err)
	} else {
		k.driver = driver
	}

	log.V(2).Infof("Initialize executor driver...")

	k.BirthCry()
	executor.KillKubeletContainers(kc.DockerClient)

	go k.GarbageCollectLoop()
	// go k.MonitorCAdvisor(kc.CAdvisorPort) // TODO(jdef) support cadvisor at some point

	return k, pc, nil
}

// kubelet decorator
type kubeletExecutor struct {
	*kubelet.Kubelet
	initialize     sync.Once
	driver         bindings.ExecutorDriver
	finished       chan struct{} // closed once driver.Run() completes
	runProxy       bool
	proxyLogV      int
	proxyExec      string
	proxyLogfile   string
	proxyBindall   bool
	address        util.IP
	etcdServerList util.StringList
	etcdConfigFile string
}

func (kl *kubeletExecutor) ListenAndServe(address net.IP, port uint, enableDebuggingHandlers bool) {
	// this func could be called many times, depending how often the HTTP server crashes,
	// so only execute certain initialization procs once
	kl.initialize.Do(func() {
		if kl.runProxy {
			go util.Forever(kl.runProxyService, 5*time.Second)
		}
		go func() {
			defer close(kl.finished)
			if _, err := kl.driver.Run(); err != nil {
				log.Fatalf("failed to start executor driver: %v", err)
			}
			log.Info("executor Run completed")
		}()

		// TODO(who?) Recover running containers from check pointed pod list.
		// @see reconcileTasks
	})
	log.Infof("Starting kubelet server...")
	kubelet.ListenAndServeKubeletServer(kl, address, port, enableDebuggingHandlers)
}

// this function blocks as long as the proxy service is running; intended to be
// executed asynchronously.
func (kl *kubeletExecutor) runProxyService() {
	// TODO(jdef): would be nice if we could run the proxy via an in-memory
	// kubelet config source (in case it crashes, kubelet would restart it);
	// not sure that k8s supports host-networking space for pods
	log.Infof("Starting proxy process...")

	bindAddress := "0.0.0.0"
	if !kl.proxyBindall {
		bindAddress = kl.address.String()
	}
	args := []string{
		fmt.Sprintf("--bind_address=%s", bindAddress),
		fmt.Sprintf("--v=%d", kl.proxyLogV),
		"--logtostderr=true",
	}
	if len(kl.etcdServerList) > 0 {
		etcdServerArguments := strings.Join(kl.etcdServerList, ",")
		args = append(args, "--etcd_servers="+etcdServerArguments)
	} else if kl.etcdConfigFile != "" {
		args = append(args, "--etcd_config="+kl.etcdConfigFile)
	}

	log.Infof("Spawning process executable %s with args '%+v'", kl.proxyExec, args)

	cmd := exec.Command(kl.proxyExec, args...)
	_, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}

	proxylogs, err := cmd.StderrPipe()
	if err != nil {
		log.Fatal(err)
	}

	//TODO(jdef) append instead of truncate? what if the disk is full?
	logfile, err := os.Create(kl.proxyLogfile)
	if err != nil {
		log.Fatal(err)
	}
	defer logfile.Close()

	go func() {
		defer func() {
			log.Infof("killing proxy process..")
			if err = cmd.Process.Kill(); err != nil {
				log.Errorf("failed to kill proxy process: %v", err)
			}
		}()

		writer := bufio.NewWriter(logfile)
		defer writer.Flush()

		written, err := io.Copy(writer, proxylogs)
		if err != nil {
			log.Errorf("error writing data to proxy log: %v", err)
		}

		log.Infof("wrote %d bytes to proxy log", written)
	}()

	// if the proxy fails to start then we exit the executor, otherwise
	// wait for the proxy process to end (and release resources after).
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	} else if err := cmd.Wait(); err != nil {
		log.Error(err)
	}
}
