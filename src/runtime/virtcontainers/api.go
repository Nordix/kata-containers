// Copyright (c) 2016 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package virtcontainers

import (
	"context"
	"os"
	"runtime"
	"syscall"

	deviceApi "github.com/kata-containers/kata-containers/src/runtime/virtcontainers/device/api"
	deviceConfig "github.com/kata-containers/kata-containers/src/runtime/virtcontainers/device/config"
	"github.com/kata-containers/kata-containers/src/runtime/virtcontainers/persist"
	"github.com/kata-containers/kata-containers/src/runtime/virtcontainers/pkg/cgroups"
	"github.com/kata-containers/kata-containers/src/runtime/virtcontainers/pkg/compatoci"
	vcTypes "github.com/kata-containers/kata-containers/src/runtime/virtcontainers/pkg/types"
	"github.com/kata-containers/kata-containers/src/runtime/virtcontainers/types"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/sirupsen/logrus"
)

func init() {
	runtime.LockOSThread()
}

var virtLog = logrus.WithField("source", "virtcontainers")

// trace creates a new tracing span based on the specified name and parent
// context.
func trace(parent context.Context, name string) (opentracing.Span, context.Context) {
	span, ctx := opentracing.StartSpanFromContext(parent, name)

	// Should not need to be changed (again).
	span.SetTag("source", "virtcontainers")
	span.SetTag("component", "virtcontainers")

	// Should be reset as new subsystems are entered.
	span.SetTag("subsystem", "api")

	return span, ctx
}

// SetLogger sets the logger for virtcontainers package.
func SetLogger(ctx context.Context, logger *logrus.Entry) {
	fields := virtLog.Data
	virtLog = logger.WithFields(fields)

	deviceApi.SetLogger(virtLog)
	compatoci.SetLogger(virtLog)
	deviceConfig.SetLogger(virtLog)
	cgroups.SetLogger(virtLog)
}

// CreateSandbox is the virtcontainers sandbox creation entry point.
// CreateSandbox creates a sandbox and its containers. It does not start them.
func CreateSandbox(ctx context.Context, sandboxConfig SandboxConfig, factory Factory) (VCSandbox, error) {
	span, ctx := trace(ctx, "CreateSandbox")
	defer span.Finish()

	s, err := createSandboxFromConfig(ctx, sandboxConfig, factory)

	return s, err
}

func createSandboxFromConfig(ctx context.Context, sandboxConfig SandboxConfig, factory Factory) (_ *Sandbox, err error) {
	span, ctx := trace(ctx, "createSandboxFromConfig")
	defer span.Finish()

	// Create the sandbox.
	s, err := createSandbox(ctx, sandboxConfig, factory)
	if err != nil {
		return nil, err
	}

	// cleanup sandbox resources in case of any failure
	defer func() {
		if err != nil {
			s.Delete()
		}
	}()

	// Create the sandbox network
	if err = s.createNetwork(); err != nil {
		return nil, err
	}

	// network rollback
	defer func() {
		if err != nil {
			s.removeNetwork()
		}
	}()

	// Move runtime to sandbox cgroup so all process are created there.
	if s.config.SandboxCgroupOnly {
		if err := s.createCgroupManager(); err != nil {
			return nil, err
		}

		if err := s.setupSandboxCgroup(); err != nil {
			return nil, err
		}
	}

	// Start the VM
	if err = s.startVM(); err != nil {
		return nil, err
	}

	// rollback to stop VM if error occurs
	defer func() {
		if err != nil {
			s.stopVM()
		}
	}()

	s.postCreatedNetwork()

	if err = s.getAndStoreGuestDetails(); err != nil {
		return nil, err
	}

	// Create Containers
	if err = s.createContainers(); err != nil {
		return nil, err
	}

	// The sandbox is completely created now, we can store it.
	if err = s.storeSandbox(); err != nil {
		return nil, err
	}

	return s, nil
}

// DeleteSandbox is the virtcontainers sandbox deletion entry point.
// DeleteSandbox will stop an already running container and then delete it.
func DeleteSandbox(ctx context.Context, sandboxID string) (VCSandbox, error) {
	span, ctx := trace(ctx, "DeleteSandbox")
	defer span.Finish()

	if sandboxID == "" {
		return nil, vcTypes.ErrNeedSandboxID
	}

	unlock, err := rwLockSandbox(sandboxID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Fetch the sandbox from storage and create it.
	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}

	// Delete it.
	if err := s.Delete(); err != nil {
		return nil, err
	}

	return s, nil
}

// FetchSandbox is the virtcontainers sandbox fetching entry point.
// FetchSandbox will find out and connect to an existing sandbox and
// return the sandbox structure. The caller is responsible of calling
// VCSandbox.Release() after done with it.
func FetchSandbox(ctx context.Context, sandboxID string) (VCSandbox, error) {
	span, ctx := trace(ctx, "FetchSandbox")
	defer span.Finish()

	if sandboxID == "" {
		return nil, vcTypes.ErrNeedSandboxID
	}

	unlock, err := rwLockSandbox(sandboxID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Fetch the sandbox from storage and create it.
	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}

	// If the agent is long live connection, it needs to restart the proxy to
	// watch the guest console if it hadn't been watched.
	if s.agent.longLiveConn() {
		err = s.startProxy()
		if err != nil {
			s.Release()
			return nil, err
		}
	}

	return s, nil
}

// StartSandbox is the virtcontainers sandbox starting entry point.
// StartSandbox will talk to the given hypervisor to start an existing
// sandbox and all its containers.
// It returns the sandbox ID.
func StartSandbox(ctx context.Context, sandboxID string) (VCSandbox, error) {
	span, ctx := trace(ctx, "StartSandbox")
	defer span.Finish()

	if sandboxID == "" {
		return nil, vcTypes.ErrNeedSandboxID
	}

	unlock, err := rwLockSandbox(sandboxID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Fetch the sandbox from storage and create it.
	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}

	// Start it
	err = s.Start()
	if err != nil {
		return nil, err
	}

	if err = s.storeSandbox(); err != nil {
		return nil, err
	}

	return s, nil
}

// StopSandbox is the virtcontainers sandbox stopping entry point.
// StopSandbox will talk to the given agent to stop an existing sandbox and destroy all containers within that sandbox.
func StopSandbox(ctx context.Context, sandboxID string, force bool) (VCSandbox, error) {
	span, ctx := trace(ctx, "StopSandbox")
	defer span.Finish()

	if sandboxID == "" {
		return nil, vcTypes.ErrNeedSandbox
	}

	unlock, err := rwLockSandbox(sandboxID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Fetch the sandbox from storage and create it.
	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}

	// Stop it.
	err = s.Stop(force)
	if err != nil {
		return nil, err
	}

	if err = s.storeSandbox(); err != nil {
		return nil, err
	}

	return s, nil
}

// RunSandbox is the virtcontainers sandbox running entry point.
// RunSandbox creates a sandbox and its containers and then it starts them.
func RunSandbox(ctx context.Context, sandboxConfig SandboxConfig, factory Factory) (VCSandbox, error) {
	span, ctx := trace(ctx, "RunSandbox")
	defer span.Finish()

	// Create the sandbox
	s, err := createSandboxFromConfig(ctx, sandboxConfig, factory)
	if err != nil {
		return nil, err
	}

	unlock, err := rwLockSandbox(s.id)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Start the sandbox
	err = s.Start()
	if err != nil {
		return nil, err
	}

	return s, nil
}

// ListSandbox is the virtcontainers sandbox listing entry point.
func ListSandbox(ctx context.Context) ([]SandboxStatus, error) {
	span, ctx := trace(ctx, "ListSandbox")
	defer span.Finish()

	store, err := persist.GetDriver()
	if err != nil {
		return []SandboxStatus{}, err
	}

	dir, err := os.Open(store.RunStoragePath())
	if err != nil {
		if os.IsNotExist(err) {
			// No sandbox directory is not an error
			return []SandboxStatus{}, nil
		}
		return []SandboxStatus{}, err
	}

	defer dir.Close()

	sandboxesID, err := dir.Readdirnames(0)
	if err != nil {
		return []SandboxStatus{}, err
	}

	var sandboxStatusList []SandboxStatus

	for _, sandboxID := range sandboxesID {
		sandboxStatus, err := StatusSandbox(ctx, sandboxID)
		if err != nil {
			continue
		}

		sandboxStatusList = append(sandboxStatusList, sandboxStatus)
	}

	return sandboxStatusList, nil
}

// StatusSandbox is the virtcontainers sandbox status entry point.
func StatusSandbox(ctx context.Context, sandboxID string) (SandboxStatus, error) {
	span, ctx := trace(ctx, "StatusSandbox")
	defer span.Finish()

	if sandboxID == "" {
		return SandboxStatus{}, vcTypes.ErrNeedSandboxID
	}

	unlock, err := rwLockSandbox(sandboxID)
	if err != nil {
		return SandboxStatus{}, err
	}
	defer unlock()

	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return SandboxStatus{}, err
	}

	var contStatusList []ContainerStatus
	for _, container := range s.containers {
		contStatus, err := statusContainer(s, container.id)
		if err != nil {
			return SandboxStatus{}, err
		}

		contStatusList = append(contStatusList, contStatus)
	}

	sandboxStatus := SandboxStatus{
		ID:               s.id,
		State:            s.state,
		Hypervisor:       s.config.HypervisorType,
		HypervisorConfig: s.config.HypervisorConfig,
		ContainersStatus: contStatusList,
		Annotations:      s.config.Annotations,
	}

	return sandboxStatus, nil
}

// CreateContainer is the virtcontainers container creation entry point.
// CreateContainer creates a container on a given sandbox.
func CreateContainer(ctx context.Context, sandboxID string, containerConfig ContainerConfig) (VCSandbox, VCContainer, error) {
	span, ctx := trace(ctx, "CreateContainer")
	defer span.Finish()

	if sandboxID == "" {
		return nil, nil, vcTypes.ErrNeedSandboxID
	}

	unlock, err := rwLockSandbox(sandboxID)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()

	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return nil, nil, err
	}

	c, err := s.CreateContainer(containerConfig)
	if err != nil {
		return nil, nil, err
	}

	if err = s.storeSandbox(); err != nil {
		return nil, nil, err
	}

	return s, c, nil
}

// DeleteContainer is the virtcontainers container deletion entry point.
// DeleteContainer deletes a Container from a Sandbox. If the container is running,
// it needs to be stopped first.
func DeleteContainer(ctx context.Context, sandboxID, containerID string) (VCContainer, error) {
	span, ctx := trace(ctx, "DeleteContainer")
	defer span.Finish()

	if sandboxID == "" {
		return nil, vcTypes.ErrNeedSandboxID
	}

	if containerID == "" {
		return nil, vcTypes.ErrNeedContainerID
	}

	unlock, err := rwLockSandbox(sandboxID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}

	return s.DeleteContainer(containerID)
}

// StartContainer is the virtcontainers container starting entry point.
// StartContainer starts an already created container.
func StartContainer(ctx context.Context, sandboxID, containerID string) (VCContainer, error) {
	span, ctx := trace(ctx, "StartContainer")
	defer span.Finish()

	if sandboxID == "" {
		return nil, vcTypes.ErrNeedSandboxID
	}

	if containerID == "" {
		return nil, vcTypes.ErrNeedContainerID
	}

	unlock, err := rwLockSandbox(sandboxID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}

	return s.StartContainer(containerID)
}

// StopContainer is the virtcontainers container stopping entry point.
// StopContainer stops an already running container.
func StopContainer(ctx context.Context, sandboxID, containerID string) (VCContainer, error) {
	span, ctx := trace(ctx, "StopContainer")
	defer span.Finish()

	if sandboxID == "" {
		return nil, vcTypes.ErrNeedSandboxID
	}

	if containerID == "" {
		return nil, vcTypes.ErrNeedContainerID
	}

	unlock, err := rwLockSandbox(sandboxID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}

	return s.StopContainer(containerID, false)
}

// EnterContainer is the virtcontainers container command execution entry point.
// EnterContainer enters an already running container and runs a given command.
func EnterContainer(ctx context.Context, sandboxID, containerID string, cmd types.Cmd) (VCSandbox, VCContainer, *Process, error) {
	span, ctx := trace(ctx, "EnterContainer")
	defer span.Finish()

	if sandboxID == "" {
		return nil, nil, nil, vcTypes.ErrNeedSandboxID
	}

	if containerID == "" {
		return nil, nil, nil, vcTypes.ErrNeedContainerID
	}

	unlock, err := rLockSandbox(sandboxID)
	if err != nil {
		return nil, nil, nil, err
	}
	defer unlock()

	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return nil, nil, nil, err
	}

	c, process, err := s.EnterContainer(containerID, cmd)
	if err != nil {
		return nil, nil, nil, err
	}

	return s, c, process, nil
}

// StatusContainer is the virtcontainers container status entry point.
// StatusContainer returns a detailed container status.
func StatusContainer(ctx context.Context, sandboxID, containerID string) (ContainerStatus, error) {
	span, ctx := trace(ctx, "StatusContainer")
	defer span.Finish()

	if sandboxID == "" {
		return ContainerStatus{}, vcTypes.ErrNeedSandboxID
	}

	if containerID == "" {
		return ContainerStatus{}, vcTypes.ErrNeedContainerID
	}

	unlock, err := rwLockSandbox(sandboxID)
	if err != nil {
		return ContainerStatus{}, err
	}
	defer unlock()

	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return ContainerStatus{}, err
	}

	return statusContainer(s, containerID)
}

func statusContainer(sandbox *Sandbox, containerID string) (ContainerStatus, error) {
	if container, ok := sandbox.containers[containerID]; ok {
		return ContainerStatus{
			ID:          container.id,
			State:       container.state,
			PID:         container.process.Pid,
			StartTime:   container.process.StartTime,
			RootFs:      container.config.RootFs.Target,
			Spec:        container.GetPatchedOCISpec(),
			Annotations: container.config.Annotations,
		}, nil
	}

	// No matching containers in the sandbox
	return ContainerStatus{}, nil
}

// KillContainer is the virtcontainers entry point to send a signal
// to a container running inside a sandbox. If all is true, all processes in
// the container will be sent the signal.
func KillContainer(ctx context.Context, sandboxID, containerID string, signal syscall.Signal, all bool) error {
	span, ctx := trace(ctx, "KillContainer")
	defer span.Finish()

	if sandboxID == "" {
		return vcTypes.ErrNeedSandboxID
	}

	if containerID == "" {
		return vcTypes.ErrNeedContainerID
	}

	unlock, err := rwLockSandbox(sandboxID)
	if err != nil {
		return err
	}
	defer unlock()

	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return err
	}

	return s.KillContainer(containerID, signal, all)
}

// ProcessListContainer is the virtcontainers entry point to list
// processes running inside a container
func ProcessListContainer(ctx context.Context, sandboxID, containerID string, options ProcessListOptions) (ProcessList, error) {
	span, ctx := trace(ctx, "ProcessListContainer")
	defer span.Finish()

	if sandboxID == "" {
		return nil, vcTypes.ErrNeedSandboxID
	}

	if containerID == "" {
		return nil, vcTypes.ErrNeedContainerID
	}

	unlock, err := rLockSandbox(sandboxID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}

	return s.ProcessListContainer(containerID, options)
}

// UpdateContainer is the virtcontainers entry point to update
// container's resources.
func UpdateContainer(ctx context.Context, sandboxID, containerID string, resources specs.LinuxResources) error {
	span, ctx := trace(ctx, "UpdateContainer")
	defer span.Finish()

	if sandboxID == "" {
		return vcTypes.ErrNeedSandboxID
	}

	if containerID == "" {
		return vcTypes.ErrNeedContainerID
	}

	unlock, err := rwLockSandbox(sandboxID)
	if err != nil {
		return err
	}
	defer unlock()

	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return err
	}

	return s.UpdateContainer(containerID, resources)
}

// StatsContainer is the virtcontainers container stats entry point.
// StatsContainer returns a detailed container stats.
func StatsContainer(ctx context.Context, sandboxID, containerID string) (ContainerStats, error) {
	span, ctx := trace(ctx, "StatsContainer")
	defer span.Finish()

	if sandboxID == "" {
		return ContainerStats{}, vcTypes.ErrNeedSandboxID
	}

	if containerID == "" {
		return ContainerStats{}, vcTypes.ErrNeedContainerID
	}

	unlock, err := rLockSandbox(sandboxID)
	if err != nil {
		return ContainerStats{}, err
	}
	defer unlock()

	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return ContainerStats{}, err
	}

	return s.StatsContainer(containerID)
}

// StatsSandbox is the virtcontainers sandbox stats entry point.
// StatsSandbox returns a detailed sandbox stats.
func StatsSandbox(ctx context.Context, sandboxID string) (SandboxStats, []ContainerStats, error) {
	span, ctx := trace(ctx, "StatsSandbox")
	defer span.Finish()

	if sandboxID == "" {
		return SandboxStats{}, []ContainerStats{}, vcTypes.ErrNeedSandboxID
	}

	unlock, err := rLockSandbox(sandboxID)
	if err != nil {
		return SandboxStats{}, []ContainerStats{}, err
	}
	defer unlock()

	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return SandboxStats{}, []ContainerStats{}, err
	}

	sandboxStats, err := s.Stats()
	if err != nil {
		return SandboxStats{}, []ContainerStats{}, err
	}

	containerStats := []ContainerStats{}
	for _, c := range s.containers {
		cstats, err := s.StatsContainer(c.id)
		if err != nil {
			return SandboxStats{}, []ContainerStats{}, err
		}
		containerStats = append(containerStats, cstats)
	}

	return sandboxStats, containerStats, nil
}

func togglePauseContainer(ctx context.Context, sandboxID, containerID string, pause bool) error {
	if sandboxID == "" {
		return vcTypes.ErrNeedSandboxID
	}

	if containerID == "" {
		return vcTypes.ErrNeedContainerID
	}

	unlock, err := rwLockSandbox(sandboxID)
	if err != nil {
		return err
	}
	defer unlock()

	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return err
	}

	if pause {
		return s.PauseContainer(containerID)
	}

	return s.ResumeContainer(containerID)
}

// PauseContainer is the virtcontainers container pause entry point.
func PauseContainer(ctx context.Context, sandboxID, containerID string) error {
	span, ctx := trace(ctx, "PauseContainer")
	defer span.Finish()

	return togglePauseContainer(ctx, sandboxID, containerID, true)
}

// ResumeContainer is the virtcontainers container resume entry point.
func ResumeContainer(ctx context.Context, sandboxID, containerID string) error {
	span, ctx := trace(ctx, "ResumeContainer")
	defer span.Finish()

	return togglePauseContainer(ctx, sandboxID, containerID, false)
}

// AddDevice will add a device to sandbox
func AddDevice(ctx context.Context, sandboxID string, info deviceConfig.DeviceInfo) (deviceApi.Device, error) {
	span, ctx := trace(ctx, "AddDevice")
	defer span.Finish()

	if sandboxID == "" {
		return nil, vcTypes.ErrNeedSandboxID
	}

	unlock, err := rwLockSandbox(sandboxID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}

	return s.AddDevice(info)
}

func toggleInterface(ctx context.Context, sandboxID string, inf *vcTypes.Interface, add bool) (*vcTypes.Interface, error) {
	if sandboxID == "" {
		return nil, vcTypes.ErrNeedSandboxID
	}

	unlock, err := rwLockSandbox(sandboxID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}

	if add {
		return s.AddInterface(inf)
	}

	return s.RemoveInterface(inf)
}

// AddInterface is the virtcontainers add interface entry point.
func AddInterface(ctx context.Context, sandboxID string, inf *vcTypes.Interface) (*vcTypes.Interface, error) {
	span, ctx := trace(ctx, "AddInterface")
	defer span.Finish()

	return toggleInterface(ctx, sandboxID, inf, true)
}

// RemoveInterface is the virtcontainers remove interface entry point.
func RemoveInterface(ctx context.Context, sandboxID string, inf *vcTypes.Interface) (*vcTypes.Interface, error) {
	span, ctx := trace(ctx, "RemoveInterface")
	defer span.Finish()

	return toggleInterface(ctx, sandboxID, inf, false)
}

// ListInterfaces is the virtcontainers list interfaces entry point.
func ListInterfaces(ctx context.Context, sandboxID string) ([]*vcTypes.Interface, error) {
	span, ctx := trace(ctx, "ListInterfaces")
	defer span.Finish()

	if sandboxID == "" {
		return nil, vcTypes.ErrNeedSandboxID
	}

	unlock, err := rLockSandbox(sandboxID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}

	return s.ListInterfaces()
}

// UpdateRoutes is the virtcontainers update routes entry point.
func UpdateRoutes(ctx context.Context, sandboxID string, routes []*vcTypes.Route) ([]*vcTypes.Route, error) {
	span, ctx := trace(ctx, "UpdateRoutes")
	defer span.Finish()

	if sandboxID == "" {
		return nil, vcTypes.ErrNeedSandboxID
	}

	unlock, err := rwLockSandbox(sandboxID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}

	return s.UpdateRoutes(routes)
}

// ListRoutes is the virtcontainers list routes entry point.
func ListRoutes(ctx context.Context, sandboxID string) ([]*vcTypes.Route, error) {
	span, ctx := trace(ctx, "ListRoutes")
	defer span.Finish()

	if sandboxID == "" {
		return nil, vcTypes.ErrNeedSandboxID
	}

	unlock, err := rLockSandbox(sandboxID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}

	return s.ListRoutes()
}

// CleanupContaienr is used by shimv2 to stop and delete a container exclusively, once there is no container
// in the sandbox left, do stop the sandbox and delete it. Those serial operations will be done exclusively by
// locking the sandbox.
func CleanupContainer(ctx context.Context, sandboxID, containerID string, force bool) error {
	span, ctx := trace(ctx, "CleanupContainer")
	defer span.Finish()

	if sandboxID == "" {
		return vcTypes.ErrNeedSandboxID
	}

	if containerID == "" {
		return vcTypes.ErrNeedContainerID
	}

	unlock, err := rwLockSandbox(sandboxID)
	if err != nil {
		return err
	}
	defer unlock()

	s, err := fetchSandbox(ctx, sandboxID)
	if err != nil {
		return err
	}

	defer s.Release()

	_, err = s.StopContainer(containerID, force)
	if err != nil && !force {
		return err
	}

	_, err = s.DeleteContainer(containerID)
	if err != nil && !force {
		return err
	}

	if len(s.GetAllContainers()) > 0 {
		return nil
	}

	if err = s.Stop(force); err != nil && !force {
		return err
	}

	if err = s.Delete(); err != nil {
		return err
	}

	return nil
}
