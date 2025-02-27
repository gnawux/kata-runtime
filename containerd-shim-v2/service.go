// Copyright (c) 2018 HyperHQ Inc.
//
// SPDX-License-Identifier: Apache-2.0
//

package containerdshim

import (
	"context"
	"io/ioutil"
	"os"
	sysexec "os/exec"
	"sync"
	"syscall"
	"time"

	eventstypes "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/events"
	"github.com/containerd/containerd/namespaces"
	cdruntime "github.com/containerd/containerd/runtime"
	cdshim "github.com/containerd/containerd/runtime/v2/shim"
	taskAPI "github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/typeurl"
	ptypes "github.com/gogo/protobuf/types"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	"github.com/kata-containers/runtime/pkg/katautils"
	vc "github.com/kata-containers/runtime/virtcontainers"
	"github.com/kata-containers/runtime/virtcontainers/pkg/compatoci"
	"github.com/kata-containers/runtime/virtcontainers/pkg/oci"
	"github.com/kata-containers/runtime/virtcontainers/types"
)

const (
	// Define the service's channel size, which is used for
	// reaping the exited processes exit state and forwarding
	// it to containerd as the containerd event format.
	bufferSize = 32

	chSize      = 128
	exitCode255 = 255

	// A time span used to wait for publish a containerd event,
	// once it costs a longer time than timeOut, it will be canceld.
	timeOut = 5 * time.Second
)

var (
	empty                     = &ptypes.Empty{}
	_     taskAPI.TaskService = (taskAPI.TaskService)(&service{})
)

// concrete virtcontainer implementation
var vci vc.VC = &vc.VCImpl{}

// New returns a new shim service that can be used via GRPC
func New(ctx context.Context, id string, publisher events.Publisher) (cdshim.Shim, error) {
	logger := logrus.WithField("ID", id)
	// Discard the log before shim init its log output. Otherwise
	// it will output into stdio, from which containerd would like
	// to get the shim's socket address.
	logrus.SetOutput(ioutil.Discard)
	vci.SetLogger(ctx, logger)
	katautils.SetLogger(ctx, logger, logger.Logger.Level)

	ctx, cancel := context.WithCancel(ctx)

	s := &service{
		id:         id,
		pid:        uint32(os.Getpid()),
		ctx:        ctx,
		containers: make(map[string]*container),
		events:     make(chan interface{}, chSize),
		ec:         make(chan exit, bufferSize),
		cancel:     cancel,
		mount:      false,
	}

	go s.processExits()

	go s.forward(publisher)

	return s, nil
}

type exit struct {
	id        string
	execid    string
	pid       uint32
	status    int
	timestamp time.Time
}

// service is the shim implementation of a remote shim over GRPC
type service struct {
	mu          sync.Mutex
	eventSendMu sync.Mutex

	// pid Since this shimv2 cannot get the container processes pid from VM,
	// thus for the returned values needed pid, just return this shim's
	// pid directly.
	pid uint32

	// if the container's rootfs is block device backed, kata shimv2
	// will not do the rootfs mount.
	mount bool

	ctx        context.Context
	sandbox    vc.VCSandbox
	containers map[string]*container
	config     *oci.RuntimeConfig
	events     chan interface{}
	monitor    chan error

	cancel func()

	ec chan exit
	id string
}

func newCommand(ctx context.Context, containerdBinary, id, containerdAddress string) (*sysexec.Cmd, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	args := []string{
		"-namespace", ns,
		"-address", containerdAddress,
		"-publish-binary", containerdBinary,
		"-id", id,
		"-debug",
	}
	cmd := sysexec.Command(self, args...)
	cmd.Dir = cwd

	// Set the go max process to 2 in case the shim forks too much process
	cmd.Env = append(os.Environ(), "GOMAXPROCS=2")

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	return cmd, nil
}

// StartShim willl start a kata shimv2 daemon which will implemented the
// ShimV2 APIs such as create/start/update etc containers.
func (s *service) StartShim(ctx context.Context, id, containerdBinary, containerdAddress string) (string, error) {
	bundlePath, err := os.Getwd()
	if err != nil {
		return "", err
	}

	address, err := getAddress(ctx, bundlePath, id)
	if err != nil {
		return "", err
	}
	if address != "" {
		if err := cdshim.WriteAddress("address", address); err != nil {
			return "", err
		}
		return address, nil
	}

	cmd, err := newCommand(ctx, containerdBinary, id, containerdAddress)
	if err != nil {
		return "", err
	}

	address, err = cdshim.SocketAddress(ctx, id)
	if err != nil {
		return "", err
	}

	socket, err := cdshim.NewSocket(address)
	if err != nil {
		return "", err
	}
	defer socket.Close()
	f, err := socket.File()
	if err != nil {
		return "", err
	}
	defer f.Close()

	cmd.ExtraFiles = append(cmd.ExtraFiles, f)

	if err := cmd.Start(); err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			cmd.Process.Kill()
		}
	}()

	// make sure to wait after start
	go cmd.Wait()
	if err := cdshim.WritePidFile("shim.pid", cmd.Process.Pid); err != nil {
		return "", err
	}
	if err := cdshim.WriteAddress("address", address); err != nil {
		return "", err
	}
	return address, nil
}

func (s *service) forward(publisher events.Publisher) {
	for e := range s.events {
		ctx, cancel := context.WithTimeout(s.ctx, timeOut)
		err := publisher.Publish(ctx, getTopic(e), e)
		cancel()
		if err != nil {
			logrus.WithError(err).Error("post event")
		}
	}
}

func (s *service) send(evt interface{}) {
	// for unit test, it will not initialize s.events
	if s.events != nil {
		s.events <- evt
	}
}

func (s *service) sendL(evt interface{}) {
	s.eventSendMu.Lock()
	if s.events != nil {
		s.events <- evt
	}
	s.eventSendMu.Unlock()
}

func getTopic(e interface{}) string {
	switch e.(type) {
	case *eventstypes.TaskCreate:
		return cdruntime.TaskCreateEventTopic
	case *eventstypes.TaskStart:
		return cdruntime.TaskStartEventTopic
	case *eventstypes.TaskOOM:
		return cdruntime.TaskOOMEventTopic
	case *eventstypes.TaskExit:
		return cdruntime.TaskExitEventTopic
	case *eventstypes.TaskDelete:
		return cdruntime.TaskDeleteEventTopic
	case *eventstypes.TaskExecAdded:
		return cdruntime.TaskExecAddedEventTopic
	case *eventstypes.TaskExecStarted:
		return cdruntime.TaskExecStartedEventTopic
	case *eventstypes.TaskPaused:
		return cdruntime.TaskPausedEventTopic
	case *eventstypes.TaskResumed:
		return cdruntime.TaskResumedEventTopic
	case *eventstypes.TaskCheckpointed:
		return cdruntime.TaskCheckpointedEventTopic
	default:
		logrus.Warnf("no topic for type %#v", e)
	}
	return cdruntime.TaskUnknownTopic
}

func (s *service) Cleanup(ctx context.Context) (_ *taskAPI.DeleteResponse, err error) {
	//Since the binary cleanup will return the DeleteResponse from stdout to
	//containerd, thus we must make sure there is no any outputs in stdout except
	//the returned response, thus here redirect the log to stderr in case there's
	//any log output to stdout.
	logrus.SetOutput(os.Stderr)

	defer func() {
		err = toGRPC(err)
	}()

	if s.id == "" {
		return nil, errdefs.ToGRPCf(errdefs.ErrInvalidArgument, "the container id is empty, please specify the container id")
	}

	path, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	ociSpec, err := compatoci.ParseConfigJSON(path)
	if err != nil {
		return nil, err
	}

	containerType, err := oci.ContainerType(ociSpec)
	if err != nil {
		return nil, err
	}

	switch containerType {
	case vc.PodSandbox:
		err = cleanupContainer(ctx, s.id, s.id, path)
		if err != nil {
			return nil, err
		}
	case vc.PodContainer:
		sandboxID, err := oci.SandboxID(ociSpec)
		if err != nil {
			return nil, err
		}

		err = cleanupContainer(ctx, sandboxID, s.id, path)
		if err != nil {
			return nil, err
		}
	}

	return &taskAPI.DeleteResponse{
		ExitedAt:   time.Now(),
		ExitStatus: 128 + uint32(unix.SIGKILL),
	}, nil
}

// Create a new sandbox or container with the underlying OCI runtime
func (s *service) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (_ *taskAPI.CreateTaskResponse, err error) {
	defer func() {
		err = toGRPC(err)
	}()

	s.mu.Lock()
	defer s.mu.Unlock()

	var c *container

	c, err = create(ctx, s, r)
	if err != nil {
		return nil, err
	}

	c.status = task.StatusCreated

	s.containers[r.ID] = c

	s.send(&eventstypes.TaskCreate{
		ContainerID: r.ID,
		Bundle:      r.Bundle,
		Rootfs:      r.Rootfs,
		IO: &eventstypes.TaskIO{
			Stdin:    r.Stdin,
			Stdout:   r.Stdout,
			Stderr:   r.Stderr,
			Terminal: r.Terminal,
		},
		Checkpoint: r.Checkpoint,
		Pid:        s.pid,
	})

	return &taskAPI.CreateTaskResponse{
		Pid: s.pid,
	}, nil
}

// Start a process
func (s *service) Start(ctx context.Context, r *taskAPI.StartRequest) (_ *taskAPI.StartResponse, err error) {
	defer func() {
		err = toGRPC(err)
	}()

	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	// hold the send lock so that the start events are sent before any exit events in the error case
	s.eventSendMu.Lock()
	defer s.eventSendMu.Unlock()

	//start a container
	if r.ExecID == "" {
		err = startContainer(ctx, s, c)
		if err != nil {
			return nil, errdefs.ToGRPC(err)
		}
		s.send(&eventstypes.TaskStart{
			ContainerID: c.id,
			Pid:         s.pid,
		})
	} else {
		//start an exec
		_, err = startExec(ctx, s, r.ID, r.ExecID)
		if err != nil {
			return nil, errdefs.ToGRPC(err)
		}
		s.send(&eventstypes.TaskExecStarted{
			ContainerID: c.id,
			ExecID:      r.ExecID,
			Pid:         s.pid,
		})
	}

	return &taskAPI.StartResponse{
		Pid: s.pid,
	}, nil
}

// Delete the initial process and container
func (s *service) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (_ *taskAPI.DeleteResponse, err error) {
	defer func() {
		err = toGRPC(err)
	}()

	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	if r.ExecID == "" {
		if err = deleteContainer(ctx, s, c); err != nil {
			return nil, err
		}

		s.send(&eventstypes.TaskDelete{
			ContainerID: c.id,
			Pid:         s.pid,
			ExitStatus:  c.exit,
			ExitedAt:    c.exitTime,
		})

		return &taskAPI.DeleteResponse{
			ExitStatus: c.exit,
			ExitedAt:   c.exitTime,
			Pid:        s.pid,
		}, nil
	}
	//deal with the exec case
	execs, err := c.getExec(r.ExecID)
	if err != nil {
		return nil, err
	}

	delete(c.execs, r.ExecID)

	return &taskAPI.DeleteResponse{
		ExitStatus: uint32(execs.exitCode),
		ExitedAt:   execs.exitTime,
		Pid:        s.pid,
	}, nil
}

// Exec an additional process inside the container
func (s *service) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (_ *ptypes.Empty, err error) {
	defer func() {
		err = toGRPC(err)
	}()

	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	if execs := c.execs[r.ExecID]; execs != nil {
		return nil, errdefs.ToGRPCf(errdefs.ErrAlreadyExists, "id %s", r.ExecID)
	}

	execs, err := newExec(c, r.Stdin, r.Stdout, r.Stderr, r.Terminal, r.Spec)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	c.execs[r.ExecID] = execs

	s.send(&eventstypes.TaskExecAdded{
		ContainerID: c.id,
		ExecID:      r.ExecID,
	})

	return empty, nil
}

// ResizePty of a process
func (s *service) ResizePty(ctx context.Context, r *taskAPI.ResizePtyRequest) (_ *ptypes.Empty, err error) {
	defer func() {
		err = toGRPC(err)
	}()

	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	processID := c.id
	if r.ExecID != "" {
		execs, err := c.getExec(r.ExecID)
		if err != nil {
			return nil, err
		}
		execs.tty.height = r.Height
		execs.tty.width = r.Width

		processID = execs.id

	}
	err = s.sandbox.WinsizeProcess(c.id, processID, r.Height, r.Width)
	if err != nil {
		return nil, err
	}

	return empty, err
}

// State returns runtime state information for a process
func (s *service) State(ctx context.Context, r *taskAPI.StateRequest) (_ *taskAPI.StateResponse, err error) {
	defer func() {
		err = toGRPC(err)
	}()

	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	if r.ExecID == "" {
		return &taskAPI.StateResponse{
			ID:         c.id,
			Bundle:     c.bundle,
			Pid:        s.pid,
			Status:     c.status,
			Stdin:      c.stdin,
			Stdout:     c.stdout,
			Stderr:     c.stderr,
			Terminal:   c.terminal,
			ExitStatus: c.exit,
		}, nil
	}

	//deal with exec case
	execs, err := c.getExec(r.ExecID)
	if err != nil {
		return nil, err
	}

	return &taskAPI.StateResponse{
		ID:         execs.id,
		Bundle:     c.bundle,
		Pid:        s.pid,
		Status:     execs.status,
		Stdin:      execs.tty.stdin,
		Stdout:     execs.tty.stdout,
		Stderr:     execs.tty.stderr,
		Terminal:   execs.tty.terminal,
		ExitStatus: uint32(execs.exitCode),
	}, nil

}

// Pause the container
func (s *service) Pause(ctx context.Context, r *taskAPI.PauseRequest) (_ *ptypes.Empty, err error) {
	defer func() {
		err = toGRPC(err)
	}()

	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	c.status = task.StatusPausing

	err = s.sandbox.PauseContainer(r.ID)
	if err == nil {
		c.status = task.StatusPaused
		return empty, nil
	}

	c.status, err = s.getContainerStatus(c.id)
	if err != nil {
		c.status = task.StatusUnknown
	}

	s.send(&eventstypes.TaskPaused{
		ContainerID: c.id,
	})

	return empty, err
}

// Resume the container
func (s *service) Resume(ctx context.Context, r *taskAPI.ResumeRequest) (_ *ptypes.Empty, err error) {
	defer func() {
		err = toGRPC(err)
	}()

	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	err = s.sandbox.ResumeContainer(c.id)
	if err == nil {
		c.status = task.StatusRunning
		return empty, nil
	}

	c.status, err = s.getContainerStatus(c.id)
	if err != nil {
		c.status = task.StatusUnknown
	}

	s.send(&eventstypes.TaskResumed{
		ContainerID: c.id,
	})

	return empty, err
}

// Kill a process with the provided signal
func (s *service) Kill(ctx context.Context, r *taskAPI.KillRequest) (_ *ptypes.Empty, err error) {
	defer func() {
		err = toGRPC(err)
	}()

	s.mu.Lock()
	defer s.mu.Unlock()

	signum := syscall.Signal(r.Signal)

	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	// According to CRI specs, kubelet will call StopPodSandbox()
	// at least once before calling RemovePodSandbox, and this call
	// is idempotent, and must not return an error if all relevant
	// resources have already been reclaimed. And in that call it will
	// send a SIGKILL signal first to try to stop the container, thus
	// once the container has terminated, here should ignore this signal
	// and return directly.
	if signum == syscall.SIGKILL || signum == syscall.SIGTERM {
		if c.status == task.StatusStopped {
			logrus.WithField("sandbox", s.sandbox.ID()).WithField("Container", c.id).Debug("Container has already been stopped")
			return empty, nil
		}
	}

	processID := c.id
	if r.ExecID != "" {
		execs, err := c.getExec(r.ExecID)
		if err != nil {
			return nil, err
		}
		processID = execs.id
		if processID == "" {
			logrus.WithFields(logrus.Fields{
				"sandbox":   s.sandbox.ID(),
				"Container": c.id,
				"ExecID":    r.ExecID,
			}).Debug("Id of exec process to be signalled is empty")
			return empty, errors.New("The exec process does not exist")
		}
	}

	return empty, s.sandbox.SignalProcess(c.id, processID, signum, r.All)
}

// Pids returns all pids inside the container
// Since for kata, it cannot get the process's pid from VM,
// thus only return the Shim's pid directly.
func (s *service) Pids(ctx context.Context, r *taskAPI.PidsRequest) (_ *taskAPI.PidsResponse, err error) {
	var processes []*task.ProcessInfo

	defer func() {
		err = toGRPC(err)
	}()

	pInfo := task.ProcessInfo{
		Pid: s.pid,
	}
	processes = append(processes, &pInfo)

	return &taskAPI.PidsResponse{
		Processes: processes,
	}, nil
}

// CloseIO of a process
func (s *service) CloseIO(ctx context.Context, r *taskAPI.CloseIORequest) (_ *ptypes.Empty, err error) {
	defer func() {
		err = toGRPC(err)
	}()

	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	tty := c.ttyio
	if r.ExecID != "" {
		execs, err := c.getExec(r.ExecID)
		if err != nil {
			return nil, err
		}
		tty = execs.ttyio
	}

	if tty != nil && tty.Stdin != nil {
		if err := tty.Stdin.Close(); err != nil {
			return nil, errors.Wrap(err, "close stdin")
		}
	}

	return empty, nil
}

// Checkpoint the container
func (s *service) Checkpoint(ctx context.Context, r *taskAPI.CheckpointTaskRequest) (_ *ptypes.Empty, err error) {
	defer func() {
		err = toGRPC(err)
	}()

	return nil, errdefs.ToGRPCf(errdefs.ErrNotImplemented, "service Checkpoint")
}

// Connect returns shim information such as the shim's pid
func (s *service) Connect(ctx context.Context, r *taskAPI.ConnectRequest) (_ *taskAPI.ConnectResponse, err error) {
	defer func() {
		err = toGRPC(err)
	}()

	s.mu.Lock()
	defer s.mu.Unlock()

	return &taskAPI.ConnectResponse{
		ShimPid: s.pid,
		//Since kata cannot get the container's pid in VM, thus only return the shim's pid.
		TaskPid: s.pid,
	}, nil
}

func (s *service) Shutdown(ctx context.Context, r *taskAPI.ShutdownRequest) (_ *ptypes.Empty, err error) {
	defer func() {
		err = toGRPC(err)
	}()

	s.mu.Lock()
	if len(s.containers) != 0 {
		s.mu.Unlock()
		return empty, nil
	}
	s.mu.Unlock()

	s.cancel()

	os.Exit(0)

	// This will never be called, but this is only there to make sure the
	// program can compile.
	return empty, nil
}

func (s *service) Stats(ctx context.Context, r *taskAPI.StatsRequest) (_ *taskAPI.StatsResponse, err error) {
	defer func() {
		err = toGRPC(err)
	}()

	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	data, err := marshalMetrics(s, c.id)
	if err != nil {
		return nil, err
	}

	return &taskAPI.StatsResponse{
		Stats: data,
	}, nil
}

// Update a running container
func (s *service) Update(ctx context.Context, r *taskAPI.UpdateTaskRequest) (_ *ptypes.Empty, err error) {
	defer func() {
		err = toGRPC(err)
	}()

	s.mu.Lock()
	defer s.mu.Unlock()

	var resources *specs.LinuxResources
	v, err := typeurl.UnmarshalAny(r.Resources)
	if err != nil {
		return nil, err
	}
	resources, ok := v.(*specs.LinuxResources)
	if !ok {
		return nil, errdefs.ToGRPCf(errdefs.ErrInvalidArgument, "Invalid resources type for %s", s.id)
	}

	err = s.sandbox.UpdateContainer(r.ID, *resources)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	return empty, nil
}

// Wait for a process to exit
func (s *service) Wait(ctx context.Context, r *taskAPI.WaitRequest) (_ *taskAPI.WaitResponse, err error) {
	var ret uint32

	defer func() {
		err = toGRPC(err)
	}()

	s.mu.Lock()
	c, err := s.getContainer(r.ID)
	s.mu.Unlock()

	if err != nil {
		return nil, err
	}

	//wait for container
	if r.ExecID == "" {
		ret = <-c.exitCh

		// refill the exitCh with the container process's exit code in case
		// there were other waits on this process.
		c.exitCh <- ret
	} else { //wait for exec
		execs, err := c.getExec(r.ExecID)
		if err != nil {
			return nil, err
		}
		ret = <-execs.exitCh

		// refill the exitCh with the exec process's exit code in case
		// there were other waits on this process.
		execs.exitCh <- ret
	}

	return &taskAPI.WaitResponse{
		ExitStatus: ret,
		ExitedAt:   c.exitTime,
	}, nil
}

func (s *service) processExits() {
	for e := range s.ec {
		s.checkProcesses(e)
	}
}

func (s *service) checkProcesses(e exit) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := e.execid
	if id == "" {
		id = e.id
	}

	s.sendL(&eventstypes.TaskExit{
		ContainerID: e.id,
		ID:          id,
		Pid:         e.pid,
		ExitStatus:  uint32(e.status),
		ExitedAt:    e.timestamp,
	})
}

func (s *service) getContainer(id string) (*container, error) {
	c := s.containers[id]

	if c == nil {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "container does not exist %s", id)
	}

	return c, nil
}

func (s *service) getContainerStatus(containerID string) (task.Status, error) {
	cStatus, err := s.sandbox.StatusContainer(containerID)
	if err != nil {
		return task.StatusUnknown, err
	}

	var status task.Status
	switch cStatus.State.State {
	case types.StateReady:
		status = task.StatusCreated
	case types.StateRunning:
		status = task.StatusRunning
	case types.StatePaused:
		status = task.StatusPaused
	case types.StateStopped:
		status = task.StatusStopped
	}

	return status, nil
}
