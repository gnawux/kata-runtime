// Copyright (c) 2016 Intel Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package virtcontainers

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	govmmQemu "github.com/intel/govmm/qemu"
	"github.com/kata-containers/runtime/virtcontainers/device/config"
	"github.com/kata-containers/runtime/virtcontainers/store"
	"github.com/kata-containers/runtime/virtcontainers/types"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
)

func newQemuConfig() HypervisorConfig {
	return HypervisorConfig{
		KernelPath:        testQemuKernelPath,
		ImagePath:         testQemuImagePath,
		InitrdPath:        testQemuInitrdPath,
		HypervisorPath:    testQemuPath,
		NumVCPUs:          defaultVCPUs,
		MemorySize:        defaultMemSzMiB,
		DefaultBridges:    defaultBridges,
		BlockDeviceDriver: defaultBlockDriver,
		DefaultMaxVCPUs:   defaultMaxQemuVCPUs,
		Msize9p:           defaultMsize9p,
	}
}

func testQemuKernelParameters(t *testing.T, kernelParams []Param, expected string, debug bool) {
	qemuConfig := newQemuConfig()
	qemuConfig.KernelParams = kernelParams
	assert := assert.New(t)

	if debug == true {
		qemuConfig.Debug = true
	}

	q := &qemu{
		config: qemuConfig,
		arch:   &qemuArchBase{},
	}

	params := q.kernelParameters()
	assert.Equal(params, expected)
}

func TestQemuKernelParameters(t *testing.T) {
	expectedOut := fmt.Sprintf("panic=1 nr_cpus=%d agent.use_vsock=false foo=foo bar=bar", MaxQemuVCPUs())
	params := []Param{
		{
			Key:   "foo",
			Value: "foo",
		},
		{
			Key:   "bar",
			Value: "bar",
		},
	}

	testQemuKernelParameters(t, params, expectedOut, true)
	testQemuKernelParameters(t, params, expectedOut, false)
}

func TestQemuCreateSandbox(t *testing.T) {
	qemuConfig := newQemuConfig()
	q := &qemu{}
	assert := assert.New(t)

	sandbox := &Sandbox{
		ctx: context.Background(),
		id:  "testSandbox",
		config: &SandboxConfig{
			HypervisorConfig: qemuConfig,
		},
	}

	vcStore, err := store.NewVCSandboxStore(sandbox.ctx, sandbox.id)
	assert.NoError(err)

	sandbox.store = vcStore

	// Create the hypervisor fake binary
	testQemuPath := filepath.Join(testDir, testHypervisor)
	_, err = os.Create(testQemuPath)
	assert.NoError(err)

	// Create parent dir path for hypervisor.json
	parentDir := store.SandboxConfigurationRootPath(sandbox.id)
	assert.NoError(os.MkdirAll(parentDir, store.DirMode))

	err = q.createSandbox(context.Background(), sandbox.id, NetworkNamespace{}, &sandbox.config.HypervisorConfig, sandbox.store)
	assert.NoError(err)
	assert.NoError(os.RemoveAll(parentDir))
	assert.Exactly(qemuConfig, q.config)
}

func TestQemuCreateSandboxMissingParentDirFail(t *testing.T) {
	qemuConfig := newQemuConfig()
	q := &qemu{}
	assert := assert.New(t)

	sandbox := &Sandbox{
		ctx: context.Background(),
		id:  "testSandbox",
		config: &SandboxConfig{
			HypervisorConfig: qemuConfig,
		},
	}

	vcStore, err := store.NewVCSandboxStore(sandbox.ctx, sandbox.id)
	assert.NoError(err)
	sandbox.store = vcStore

	// Create the hypervisor fake binary
	testQemuPath := filepath.Join(testDir, testHypervisor)
	_, err = os.Create(testQemuPath)
	assert.NoError(err)

	// Ensure parent dir path for hypervisor.json does not exist.
	parentDir := store.SandboxConfigurationRootPath(sandbox.id)
	assert.NoError(os.RemoveAll(parentDir))

	err = q.createSandbox(context.Background(), sandbox.id, NetworkNamespace{}, &sandbox.config.HypervisorConfig, sandbox.store)
	assert.NoError(err)
}

func TestQemuCPUTopology(t *testing.T) {
	assert := assert.New(t)
	vcpus := 1

	q := &qemu{
		arch: &qemuArchBase{},
		config: HypervisorConfig{
			NumVCPUs:        uint32(vcpus),
			DefaultMaxVCPUs: uint32(vcpus),
		},
	}

	expectedOut := govmmQemu.SMP{
		CPUs:    uint32(vcpus),
		Sockets: uint32(vcpus),
		Cores:   defaultCores,
		Threads: defaultThreads,
		MaxCPUs: uint32(vcpus),
	}

	smp := q.cpuTopology()
	assert.Exactly(smp, expectedOut)
}

func TestQemuMemoryTopology(t *testing.T) {
	mem := uint32(1000)
	slots := uint32(8)
	assert := assert.New(t)

	q := &qemu{
		arch: &qemuArchBase{},
		config: HypervisorConfig{
			MemorySize: mem,
			MemSlots:   slots,
		},
	}

	hostMemKb, err := getHostMemorySizeKb(procMemInfo)
	assert.NoError(err)
	memMax := fmt.Sprintf("%dM", int(float64(hostMemKb)/1024))

	expectedOut := govmmQemu.Memory{
		Size:   fmt.Sprintf("%dM", mem),
		Slots:  uint8(slots),
		MaxMem: memMax,
	}

	memory, err := q.memoryTopology()
	assert.NoError(err)
	assert.Exactly(memory, expectedOut)
}

func testQemuAddDevice(t *testing.T, devInfo interface{}, devType deviceType, expected []govmmQemu.Device) {
	assert := assert.New(t)
	q := &qemu{
		ctx:  context.Background(),
		arch: &qemuArchBase{},
	}

	err := q.addDevice(devInfo, devType)
	assert.NoError(err)
	assert.Exactly(q.qemuConfig.Devices, expected)
}

func TestQemuAddDeviceFsDev(t *testing.T) {
	mountTag := "testMountTag"
	hostPath := "testHostPath"

	expectedOut := []govmmQemu.Device{
		govmmQemu.FSDevice{
			Driver:        govmmQemu.Virtio9P,
			FSDriver:      govmmQemu.Local,
			ID:            fmt.Sprintf("extra-9p-%s", mountTag),
			Path:          hostPath,
			MountTag:      mountTag,
			SecurityModel: govmmQemu.None,
		},
	}

	volume := types.Volume{
		MountTag: mountTag,
		HostPath: hostPath,
	}

	testQemuAddDevice(t, volume, fsDev, expectedOut)
}

func TestQemuAddDeviceSerialPortDev(t *testing.T) {
	deviceID := "channelTest"
	id := "charchTest"
	hostPath := "/tmp/hyper_test.sock"
	name := "sh.hyper.channel.test"

	expectedOut := []govmmQemu.Device{
		govmmQemu.CharDevice{
			Driver:   govmmQemu.VirtioSerialPort,
			Backend:  govmmQemu.Socket,
			DeviceID: deviceID,
			ID:       id,
			Path:     hostPath,
			Name:     name,
		},
	}

	socket := types.Socket{
		DeviceID: deviceID,
		ID:       id,
		HostPath: hostPath,
		Name:     name,
	}

	testQemuAddDevice(t, socket, serialPortDev, expectedOut)
}

func TestQemuAddDeviceKataVSOCK(t *testing.T) {
	assert := assert.New(t)

	dir, err := ioutil.TempDir("", "")
	assert.NoError(err)
	defer os.RemoveAll(dir)

	vsockFilename := filepath.Join(dir, "vsock")

	contextID := uint64(3)
	port := uint32(1024)

	vsockFile, err := os.Create(vsockFilename)
	assert.NoError(err)
	defer vsockFile.Close()

	expectedOut := []govmmQemu.Device{
		govmmQemu.VSOCKDevice{
			ID:        fmt.Sprintf("vsock-%d", contextID),
			ContextID: contextID,
			VHostFD:   vsockFile,
		},
	}

	vsock := kataVSOCK{
		contextID: contextID,
		port:      port,
		vhostFd:   vsockFile,
	}

	testQemuAddDevice(t, vsock, vSockPCIDev, expectedOut)
}

func TestQemuGetSandboxConsole(t *testing.T) {
	assert := assert.New(t)
	q := &qemu{
		ctx: context.Background(),
	}
	sandboxID := "testSandboxID"
	expected := filepath.Join(store.RunVMStoragePath, sandboxID, consoleSocket)

	result, err := q.getSandboxConsole(sandboxID)
	assert.NoError(err)
	assert.Equal(result, expected)
}

func TestQemuCapabilities(t *testing.T) {
	assert := assert.New(t)
	q := &qemu{
		ctx:  context.Background(),
		arch: &qemuArchBase{},
	}

	caps := q.capabilities()
	assert.True(caps.IsBlockDeviceHotplugSupported())
}

func TestQemuQemuPath(t *testing.T) {
	assert := assert.New(t)

	f, err := ioutil.TempFile("", "qemu")
	assert.NoError(err)
	defer func() { _ = f.Close() }()
	defer func() { _ = os.Remove(f.Name()) }()

	expectedPath := f.Name()
	qemuConfig := newQemuConfig()
	qemuConfig.HypervisorPath = expectedPath
	qkvm := &qemuArchBase{
		machineType: "pc",
		qemuPaths: map[string]string{
			"pc": expectedPath,
		},
	}

	q := &qemu{
		config: qemuConfig,
		arch:   qkvm,
	}

	// get config hypervisor path
	path, err := q.qemuPath()
	assert.NoError(err)
	assert.Equal(path, expectedPath)

	// config hypervisor path does not exist
	q.config.HypervisorPath = "/abc/rgb/123"
	path, err = q.qemuPath()
	assert.Error(err)
	assert.Equal(path, "")

	// get arch hypervisor path
	q.config.HypervisorPath = ""
	path, err = q.qemuPath()
	assert.NoError(err)
	assert.Equal(path, expectedPath)

	// bad machine type, arch should fail
	qkvm.machineType = "rgb"
	q.arch = qkvm
	path, err = q.qemuPath()
	assert.Error(err)
	assert.Equal(path, "")
}

func TestHotplugUnsupportedDeviceType(t *testing.T) {
	assert := assert.New(t)

	qemuConfig := newQemuConfig()
	q := &qemu{
		ctx:    context.Background(),
		id:     "qemuTest",
		config: qemuConfig,
	}

	vcStore, err := store.NewVCSandboxStore(q.ctx, q.id)
	assert.NoError(err)
	q.store = vcStore

	_, err = q.hotplugAddDevice(&memoryDevice{0, 128, uint64(0), false}, fsDev)
	assert.Error(err)
	_, err = q.hotplugRemoveDevice(&memoryDevice{0, 128, uint64(0), false}, fsDev)
	assert.Error(err)
}

func TestQMPSetupShutdown(t *testing.T) {
	assert := assert.New(t)

	qemuConfig := newQemuConfig()
	q := &qemu{
		config: qemuConfig,
	}

	q.qmpShutdown()

	q.qmpMonitorCh.qmp = &govmmQemu.QMP{}
	err := q.qmpSetup()
	assert.Nil(err)
}

func TestQemuCleanup(t *testing.T) {
	assert := assert.New(t)

	q := &qemu{
		ctx:    context.Background(),
		config: newQemuConfig(),
	}

	err := q.cleanup()
	assert.Nil(err)
}

func TestQemuGrpc(t *testing.T) {
	assert := assert.New(t)

	config := newQemuConfig()
	q := &qemu{
		id:     "testqemu",
		config: config,
	}

	json, err := q.toGrpc()
	assert.Nil(err)

	var q2 qemu
	err = q2.fromGrpc(context.Background(), &config, nil, json)
	assert.Nil(err)

	assert.True(q.id == q2.id)
}

func TestQemuFileBackedMem(t *testing.T) {
	assert := assert.New(t)

	// Check default Filebackedmem location for virtio-fs
	sandbox, err := createQemuSandboxConfig()
	assert.NoError(err)

	q := &qemu{}
	sandbox.config.HypervisorConfig.SharedFS = config.VirtioFS
	err = q.createSandbox(context.Background(), sandbox.id, NetworkNamespace{}, &sandbox.config.HypervisorConfig, sandbox.store)
	assert.NoError(err)

	assert.Equal(q.qemuConfig.Knobs.FileBackedMem, true)
	assert.Equal(q.qemuConfig.Knobs.MemShared, true)
	assert.Equal(q.qemuConfig.Memory.Path, fallbackFileBackedMemDir)

	// Check failure for VM templating
	sandbox, err = createQemuSandboxConfig()
	assert.NoError(err)

	q = &qemu{}
	sandbox.config.HypervisorConfig.BootToBeTemplate = true
	sandbox.config.HypervisorConfig.SharedFS = config.VirtioFS
	sandbox.config.HypervisorConfig.MemoryPath = fallbackFileBackedMemDir

	err = q.createSandbox(context.Background(), sandbox.id, NetworkNamespace{}, &sandbox.config.HypervisorConfig, sandbox.store)

	expectErr := errors.New("VM templating has been enabled with either virtio-fs or file backed memory and this configuration will not work")
	assert.Equal(expectErr.Error(), err.Error())

	// Check Setting of non-existent shared-mem path
	sandbox, err = createQemuSandboxConfig()
	assert.NoError(err)

	q = &qemu{}
	sandbox.config.HypervisorConfig.FileBackedMemRootDir = "/tmp/xyzabc"
	err = q.createSandbox(context.Background(), sandbox.id, NetworkNamespace{}, &sandbox.config.HypervisorConfig, sandbox.store)
	assert.NoError(err)
	assert.Equal(q.qemuConfig.Knobs.FileBackedMem, false)
	assert.Equal(q.qemuConfig.Knobs.MemShared, false)
	assert.Equal(q.qemuConfig.Memory.Path, "")
}

func createQemuSandboxConfig() (*Sandbox, error) {

	qemuConfig := newQemuConfig()
	sandbox := Sandbox{
		ctx: context.Background(),
		id:  "testSandbox",
		config: &SandboxConfig{
			HypervisorConfig: qemuConfig,
		},
	}

	vcStore, err := store.NewVCSandboxStore(sandbox.ctx, sandbox.id)
	if err != nil {
		return &Sandbox{}, err
	}
	sandbox.store = vcStore

	return &sandbox, nil
}

func TestQemuVirtiofsdArgs(t *testing.T) {
	assert := assert.New(t)

	q := &qemu{
		id: "foo",
		config: HypervisorConfig{
			VirtioFSCache: "none",
			Debug:         true,
		},
	}

	savedKataHostSharedDir := kataHostSharedDir
	kataHostSharedDir = "test-share-dir"
	defer func() {
		kataHostSharedDir = savedKataHostSharedDir
	}()

	result := "-o vhost_user_socket=bar1 -o source=test-share-dir/foo -o cache=none -d"
	args := q.virtiofsdArgs("bar1")
	assert.Equal(strings.Join(args, " "), result)

	q.config.Debug = false
	result = "-o vhost_user_socket=bar2 -o source=test-share-dir/foo -o cache=none -f"
	args = q.virtiofsdArgs("bar2")
	assert.Equal(strings.Join(args, " "), result)
}

func TestQemuWaitVirtiofsd(t *testing.T) {
	assert := assert.New(t)

	q := &qemu{}

	ready := make(chan error, 1)
	timeout := 5

	ready <- nil
	remain, err := q.waitVirtiofsd(time.Now(), timeout, ready, "")
	assert.Nil(err)
	assert.True(remain <= timeout)
	assert.True(remain >= 0)

	timeout = 0
	remain, err = q.waitVirtiofsd(time.Now(), timeout, ready, "")
	assert.NotNil(err)
	assert.True(remain == 0)
}

func TestQemuGetpids(t *testing.T) {
	assert := assert.New(t)

	qemuConfig := newQemuConfig()
	q := &qemu{}
	pids := q.getPids()
	assert.NotNil(pids)
	assert.True(len(pids) == 1)
	assert.True(pids[0] == 0)

	q = &qemu{
		config: qemuConfig,
	}
	f, err := ioutil.TempFile("", "qemu-test-")
	assert.Nil(err)
	tmpfile := f.Name()
	f.Close()
	defer os.Remove(tmpfile)

	q.qemuConfig.PidFile = tmpfile
	pids = q.getPids()
	assert.True(len(pids) == 1)
	assert.True(pids[0] == 0)

	err = ioutil.WriteFile(tmpfile, []byte("100"), 0)
	assert.Nil(err)
	pids = q.getPids()
	assert.True(len(pids) == 1)
	assert.True(pids[0] == 100)

	q.state.VirtiofsdPid = 200
	pids = q.getPids()
	assert.True(len(pids) == 2)
	assert.True(pids[0] == 100)
	assert.True(pids[1] == 200)
}
