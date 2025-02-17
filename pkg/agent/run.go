package agent

import (
	"context"
	"errors"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	systemd "github.com/coreos/go-systemd/daemon"
	"github.com/rancher/k3s/pkg/agent/config"
	"github.com/rancher/k3s/pkg/agent/containerd"
	"github.com/rancher/k3s/pkg/agent/flannel"
	"github.com/rancher/k3s/pkg/agent/loadbalancer"
	"github.com/rancher/k3s/pkg/agent/netpol"
	"github.com/rancher/k3s/pkg/agent/syssetup"
	"github.com/rancher/k3s/pkg/agent/tunnel"
	"github.com/rancher/k3s/pkg/cli/cmds"
	"github.com/rancher/k3s/pkg/clientaccess"
	"github.com/rancher/k3s/pkg/daemons/agent"
	daemonconfig "github.com/rancher/k3s/pkg/daemons/config"
	"github.com/rancher/k3s/pkg/rootless"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	InternalIPLabel = "k3s.io/internal-ip"
	ExternalIPLabel = "k3s.io/external-ip"
	HostnameLabel   = "k3s.io/hostname"
)

func run(ctx context.Context, cfg cmds.Agent, lb *loadbalancer.LoadBalancer) error {
	nodeConfig := config.Get(ctx, cfg)

	if !nodeConfig.NoFlannel {
		if err := flannel.Prepare(ctx, nodeConfig); err != nil {
			return err
		}
	}

	if nodeConfig.Docker || nodeConfig.ContainerRuntimeEndpoint != "" {
		nodeConfig.AgentConfig.RuntimeSocket = nodeConfig.ContainerRuntimeEndpoint
		nodeConfig.AgentConfig.CNIPlugin = true
	} else {
		if err := containerd.Run(ctx, nodeConfig); err != nil {
			return err
		}
	}

	if err := tunnel.Setup(ctx, nodeConfig, lb.Update); err != nil {
		return err
	}

	if err := agent.Agent(&nodeConfig.AgentConfig); err != nil {
		return err
	}

	coreClient, err := coreClient(nodeConfig.AgentConfig.KubeConfigKubelet)
	if err != nil {
		return err
	}
	if !nodeConfig.NoFlannel {
		if err := flannel.Run(ctx, nodeConfig, coreClient.CoreV1().Nodes()); err != nil {
			return err
		}
	}

	if !nodeConfig.AgentConfig.DisableCCM {
		if err := syncAddressesLabels(ctx, &nodeConfig.AgentConfig, coreClient.CoreV1().Nodes()); err != nil {
			return err
		}
	}

	if !nodeConfig.AgentConfig.DisableNPC {
		if err := netpol.Run(ctx, nodeConfig); err != nil {
			return err
		}
	}

	<-ctx.Done()
	return ctx.Err()
}

func coreClient(cfg string) (kubernetes.Interface, error) {
	restConfig, err := clientcmd.BuildConfigFromFlags("", cfg)
	if err != nil {
		return nil, err
	}

	return kubernetes.NewForConfig(restConfig)
}

func Run(ctx context.Context, cfg cmds.Agent) error {
	if err := validate(); err != nil {
		return err
	}
	syssetup.Configure()

	if cfg.Rootless && !cfg.RootlessAlreadyUnshared {
		if err := rootless.Rootless(cfg.DataDir); err != nil {
			return err
		}
	}

	cfg.DataDir = filepath.Join(cfg.DataDir, "agent")
	os.MkdirAll(cfg.DataDir, 0700)

	lb, err := loadbalancer.Setup(ctx, cfg)
	if err != nil {
		return err
	}
	if lb != nil {
		// cfg.ServerURL = lb.LoadBalancerServerURL()
		if cfg.ServerURLPublic == nil {
			cfg.ServerURL = lb.LoadBalancerServerURL()
		}
	}

	for {
		newToken, err := clientaccess.NormalizeAndValidateTokenForUser(cfg.ServerURL, cfg.Token, "node")
		if err != nil {
			logrus.Error(err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		cfg.Token = newToken
		break
	}

	systemd.SdNotify(true, "READY=1\n")
	return run(ctx, cfg, lb)
}

// RunTunnelOnly is called by the server when --disable-agent=true
func RunTunnelOnly(ctx context.Context, cfg cmds.Agent) error {
	logrus.Infof("[RunTunnelOnly] made it to beginning")
	cfg.DataDir = filepath.Join(cfg.DataDir, "agent")
	os.MkdirAll(cfg.DataDir, 0700)

	cfg.DisableLoadBalancer = true
	// lb, err := loadbalancer.Setup(ctx, cfg)
	// if err != nil {
	// 	return err
	// }
	// if lb != nil {
	// 	// cfg.ServerURL = lb.LoadBalancerServerURL()
	// 	if cfg.ServerURLPublic == nil {
	// 		cfg.ServerURL = lb.LoadBalancerServerURL()
	// 	}
	// }

	for {
		newToken, err := clientaccess.NormalizeAndValidateTokenForUser(cfg.ServerURL, cfg.Token, "node")
		if err != nil {
			logrus.Error(err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		cfg.Token = newToken
		break
	}

	nodeConfig := config.Get(ctx, cfg)
	err := tunnel.Setup(ctx, nodeConfig, nil)
	return err
}

func validate() error {
	cgroups, err := ioutil.ReadFile("/proc/self/cgroup")
	if err != nil {
		return err
	}

	if !strings.Contains(string(cgroups), "cpuset") {
		logrus.Warn("Failed to find cpuset cgroup, you may need to add \"cgroup_enable=cpuset\" to your linux cmdline (/boot/cmdline.txt on a Raspberry Pi)")
	}

	if !strings.Contains(string(cgroups), "memory") {
		msg := "ailed to find memory cgroup, you may need to add \"cgroup_memory=1 cgroup_enable=memory\" to your linux cmdline (/boot/cmdline.txt on a Raspberry Pi)"
		logrus.Error("F" + msg)
		return errors.New("f" + msg)
	}

	return nil
}

func syncAddressesLabels(ctx context.Context, agentConfig *daemonconfig.Agent, nodes v1.NodeInterface) error {
	for {
		node, err := nodes.Get(agentConfig.NodeName, metav1.GetOptions{})
		if err != nil {
			logrus.Infof("Waiting for kubelet to be ready on node %s: %v", agentConfig.NodeName, err)
			time.Sleep(1 * time.Second)
			continue
		}

		newLabels, update := updateLabelMap(agentConfig, node.Labels)
		if update {
			node.Labels = newLabels
			if _, err := nodes.Update(node); err != nil {
				logrus.Infof("Failed to update node %s: %v", agentConfig.NodeName, err)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Second):
					continue
				}
			}
			logrus.Infof("addresses labels has been set successfully on node: %s", agentConfig.NodeName)
		} else {
			logrus.Infof("addresses labels has already been set successfully on node: %s", agentConfig.NodeName)
		}

		break
	}

	return nil
}

func updateLabelMap(agentConfig *daemonconfig.Agent, nodeLabels map[string]string) (map[string]string, bool) {
	result := map[string]string{}
	for k, v := range nodeLabels {
		result[k] = v
	}

	result[InternalIPLabel] = agentConfig.NodeIP
	result[HostnameLabel] = agentConfig.NodeName
	if agentConfig.NodeExternalIP == "" {
		delete(result, ExternalIPLabel)
	} else {
		result[ExternalIPLabel] = agentConfig.NodeExternalIP
	}

	return result, !equality.Semantic.DeepEqual(nodeLabels, result)
}
