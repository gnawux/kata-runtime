// +build linux
// Copyright (c) 2016 Intel Corporation
// Copyright (c) 2014,2015,2016,2017 Docker, Inc.
// SPDX-License-Identifier: Apache-2.0
//

package virtcontainers

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/containerd/cgroups"
	vcTypes "github.com/kata-containers/runtime/virtcontainers/pkg/types"
	"github.com/kata-containers/runtime/virtcontainers/types"
	"github.com/kata-containers/runtime/virtcontainers/utils"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	"github.com/kata-containers/runtime/virtcontainers/device/config"
	"github.com/kata-containers/runtime/virtcontainers/device/manager"
	"github.com/kata-containers/runtime/virtcontainers/store"
)

// https://github.com/torvalds/linux/blob/master/include/uapi/linux/major.h
// This file has definitions for major device numbers.
var cdromMajors = map[int64]string{
	11: "SCSI_CDROM_MAJOR",
	15: "CDU31A_CDROM_MAJOR",
	16: "GOLDSTAR_CDROM_MAJOR",
	17: "OPTICS_CDROM_MAJOR",
	18: "SANYO_CDROM_MAJOR",
	20: "MITSUMI_X_CDROM_MAJOR",
	23: "MITSUMI_CDROM_MAJOR",
	24: "CDU535_CDROM_MAJOR",
	25: "MATSUSHITA_CDROM_MAJOR",
	26: "MATSUSHITA_CDROM2_MAJOR",
	27: "MATSUSHITA_CDROM3_MAJOR",
	28: "MATSUSHITA_CDROM4_MAJOR",
	29: "AZTECH_CDROM_MAJOR",
	32: "CM206_CDROM_MAJOR",
}

// https://github.com/torvalds/linux/blob/master/include/uapi/linux/major.h
// #define FLOPPY_MAJOR		2
const floppyMajor = int64(2)

// Process gathers data related to a container process.
type Process struct {
	// Token is the process execution context ID. It must be
	// unique per sandbox.
	// Token is used to manipulate processes for containers
	// that have not started yet, and later identify them
	// uniquely within a sandbox.
	Token string

	// Pid is the process ID as seen by the host software
	// stack, e.g. CRI-O, containerd. This is typically the
	// shim PID.
	Pid int

	StartTime time.Time
}

// ContainerStatus describes a container status.
type ContainerStatus struct {
	ID        string
	State     types.ContainerState
	PID       int
	StartTime time.Time
	RootFs    string
	Spec      *specs.Spec

	// Annotations allow clients to store arbitrary values,
	// for example to add additional status values required
	// to support particular specifications.
	Annotations map[string]string
}

// ThrottlingData gather the date related to container cpu throttling.
type ThrottlingData struct {
	// Number of periods with throttling active
	Periods uint64 `json:"periods,omitempty"`
	// Number of periods when the container hit its throttling limit.
	ThrottledPeriods uint64 `json:"throttled_periods,omitempty"`
	// Aggregate time the container was throttled for in nanoseconds.
	ThrottledTime uint64 `json:"throttled_time,omitempty"`
}

// CPUUsage denotes the usage of a CPU.
// All CPU stats are aggregate since container inception.
type CPUUsage struct {
	// Total CPU time consumed.
	// Units: nanoseconds.
	TotalUsage uint64 `json:"total_usage,omitempty"`
	// Total CPU time consumed per core.
	// Units: nanoseconds.
	PercpuUsage []uint64 `json:"percpu_usage,omitempty"`
	// Time spent by tasks of the cgroup in kernel mode.
	// Units: nanoseconds.
	UsageInKernelmode uint64 `json:"usage_in_kernelmode"`
	// Time spent by tasks of the cgroup in user mode.
	// Units: nanoseconds.
	UsageInUsermode uint64 `json:"usage_in_usermode"`
}

// CPUStats describes the cpu stats
type CPUStats struct {
	CPUUsage       CPUUsage       `json:"cpu_usage,omitempty"`
	ThrottlingData ThrottlingData `json:"throttling_data,omitempty"`
}

// MemoryData gather the data related to memory
type MemoryData struct {
	Usage    uint64 `json:"usage,omitempty"`
	MaxUsage uint64 `json:"max_usage,omitempty"`
	Failcnt  uint64 `json:"failcnt"`
	Limit    uint64 `json:"limit"`
}

// MemoryStats describes the memory stats
type MemoryStats struct {
	// memory used for cache
	Cache uint64 `json:"cache,omitempty"`
	// usage of memory
	Usage MemoryData `json:"usage,omitempty"`
	// usage of memory  swap
	SwapUsage MemoryData `json:"swap_usage,omitempty"`
	// usage of kernel memory
	KernelUsage MemoryData `json:"kernel_usage,omitempty"`
	// usage of kernel TCP memory
	KernelTCPUsage MemoryData `json:"kernel_tcp_usage,omitempty"`
	// if true, memory usage is accounted for throughout a hierarchy of cgroups.
	UseHierarchy bool `json:"use_hierarchy"`

	Stats map[string]uint64 `json:"stats,omitempty"`
}

// PidsStats describes the pids stats
type PidsStats struct {
	// number of pids in the cgroup
	Current uint64 `json:"current,omitempty"`
	// active pids hard limit
	Limit uint64 `json:"limit,omitempty"`
}

// BlkioStatEntry gather date related to a block device
type BlkioStatEntry struct {
	Major uint64 `json:"major,omitempty"`
	Minor uint64 `json:"minor,omitempty"`
	Op    string `json:"op,omitempty"`
	Value uint64 `json:"value,omitempty"`
}

// BlkioStats describes block io stats
type BlkioStats struct {
	// number of bytes tranferred to and from the block device
	IoServiceBytesRecursive []BlkioStatEntry `json:"io_service_bytes_recursive,omitempty"`
	IoServicedRecursive     []BlkioStatEntry `json:"io_serviced_recursive,omitempty"`
	IoQueuedRecursive       []BlkioStatEntry `json:"io_queue_recursive,omitempty"`
	IoServiceTimeRecursive  []BlkioStatEntry `json:"io_service_time_recursive,omitempty"`
	IoWaitTimeRecursive     []BlkioStatEntry `json:"io_wait_time_recursive,omitempty"`
	IoMergedRecursive       []BlkioStatEntry `json:"io_merged_recursive,omitempty"`
	IoTimeRecursive         []BlkioStatEntry `json:"io_time_recursive,omitempty"`
	SectorsRecursive        []BlkioStatEntry `json:"sectors_recursive,omitempty"`
}

// HugetlbStats describes hugetable memory stats
type HugetlbStats struct {
	// current res_counter usage for hugetlb
	Usage uint64 `json:"usage,omitempty"`
	// maximum usage ever recorded.
	MaxUsage uint64 `json:"max_usage,omitempty"`
	// number of times hugetlb usage allocation failure.
	Failcnt uint64 `json:"failcnt"`
}

// CgroupStats describes all cgroup subsystem stats
type CgroupStats struct {
	CPUStats    CPUStats    `json:"cpu_stats,omitempty"`
	MemoryStats MemoryStats `json:"memory_stats,omitempty"`
	PidsStats   PidsStats   `json:"pids_stats,omitempty"`
	BlkioStats  BlkioStats  `json:"blkio_stats,omitempty"`
	// the map is in the format "size of hugepage: stats of the hugepage"
	HugetlbStats map[string]HugetlbStats `json:"hugetlb_stats,omitempty"`
}

// NetworkStats describe all network stats.
type NetworkStats struct {
	// Name is the name of the network interface.
	Name string `json:"name,omitempty"`

	RxBytes   uint64 `json:"rx_bytes,omitempty"`
	RxPackets uint64 `json:"rx_packets,omitempty"`
	RxErrors  uint64 `json:"rx_errors,omitempty"`
	RxDropped uint64 `json:"rx_dropped,omitempty"`
	TxBytes   uint64 `json:"tx_bytes,omitempty"`
	TxPackets uint64 `json:"tx_packets,omitempty"`
	TxErrors  uint64 `json:"tx_errors,omitempty"`
	TxDropped uint64 `json:"tx_dropped,omitempty"`
}

// ContainerStats describes a container stats.
type ContainerStats struct {
	CgroupStats  *CgroupStats
	NetworkStats []*NetworkStats
}

// ContainerResources describes container resources
type ContainerResources struct {
	// VCPUs are the number of vCPUs that are being used by the container
	VCPUs uint32

	// Mem is the memory that is being used by the container
	MemByte int64
}

// ContainerConfig describes one container runtime configuration.
type ContainerConfig struct {
	ID string

	// RootFs is the container workload image on the host.
	RootFs RootFs

	// ReadOnlyRootfs indicates if the rootfs should be mounted readonly
	ReadonlyRootfs bool

	// Cmd specifies the command to run on a container
	Cmd types.Cmd

	// Annotations allow clients to store arbitrary values,
	// for example to add additional status values required
	// to support particular specifications.
	Annotations map[string]string

	Mounts []Mount

	// Device configuration for devices that must be available within the container.
	DeviceInfos []config.DeviceInfo

	// Resources container resources
	Resources specs.LinuxResources

	// Raw OCI specification, it won't be saved to disk.
	Spec *specs.Spec `json:"_"`
}

// valid checks that the container configuration is valid.
func (c *ContainerConfig) valid() bool {
	if c == nil {
		return false
	}

	if c.ID == "" {
		return false
	}

	return true
}

// SystemMountsInfo describes additional information for system mounts that the agent
// needs to handle
type SystemMountsInfo struct {
	// Indicates if /dev has been passed as a bind mount for the host /dev
	BindMountDev bool

	// Size of /dev/shm assigned on the host.
	DevShmSize uint
}

// ContainerDevice describes a device associated with container
type ContainerDevice struct {
	// ID is device id referencing the device from sandbox's device manager
	ID string

	// ContainerPath is device path displayed in container
	ContainerPath string

	// FileMode permission bits for the device.
	FileMode os.FileMode

	// UID is user ID in the container namespace
	UID uint32

	// GID is group ID in the container namespace
	GID uint32
}

// RootFs describes the container's rootfs.
type RootFs struct {
	// Source specifies the BlockDevice path
	Source string
	// Target specify where the rootfs is mounted if it has been mounted
	Target string
	// Type specifies the type of filesystem to mount.
	Type string
	// Options specifies zero or more fstab style mount options.
	Options []string
	// Mounted specifies whether the rootfs has be mounted or not
	Mounted bool
}

// Container is composed of a set of containers and a runtime environment.
// A Container can be created, deleted, started, stopped, listed, entered, paused and restored.
type Container struct {
	id        string
	sandboxID string

	rootFs RootFs

	config *ContainerConfig

	sandbox *Sandbox

	runPath       string
	configPath    string
	containerPath string
	rootfsSuffix  string

	state types.ContainerState

	process Process

	mounts []Mount

	devices []ContainerDevice

	systemMountsInfo SystemMountsInfo

	ctx context.Context

	store *store.VCStore
}

// ID returns the container identifier string.
func (c *Container) ID() string {
	return c.id
}

// Logger returns a logrus logger appropriate for logging Container messages
func (c *Container) Logger() *logrus.Entry {
	return virtLog.WithFields(logrus.Fields{
		"subsystem": "container",
		"sandbox":   c.sandboxID,
	})
}

func (c *Container) trace(name string) (opentracing.Span, context.Context) {
	if c.ctx == nil {
		c.Logger().WithField("type", "bug").Error("trace called before context set")
		c.ctx = context.Background()
	}

	span, ctx := opentracing.StartSpanFromContext(c.ctx, name)

	span.SetTag("subsystem", "container")

	return span, ctx
}

// Sandbox returns the sandbox handler related to this container.
func (c *Container) Sandbox() VCSandbox {
	return c.sandbox
}

// Process returns the container process.
func (c *Container) Process() Process {
	return c.process
}

// GetToken returns the token related to this container's process.
func (c *Container) GetToken() string {
	return c.process.Token
}

// GetPid returns the pid related to this container's process.
func (c *Container) GetPid() int {
	return c.process.Pid
}

func (c *Container) setStateFstype(fstype string) error {
	c.state.Fstype = fstype

	if !c.sandbox.supportNewStore() {
		// experimental runtime use "persist.json" which doesn't need "state.json" anymore
		if err := c.storeState(); err != nil {
			return err
		}
	}

	return nil
}

// GetAnnotations returns container's annotations
func (c *Container) GetAnnotations() map[string]string {
	return c.config.Annotations
}

// GetOCISpec returns container's OCI specification
func (c *Container) GetOCISpec() *specs.Spec {
	return c.config.Spec
}

// storeContainer stores a container config.
func (c *Container) storeContainer() error {
	if c.sandbox.supportNewStore() {
		if err := c.sandbox.Save(); err != nil {
			return err
		}
	}
	return c.store.Store(store.Configuration, *(c.config))
}

func (c *Container) storeProcess() error {
	return c.store.Store(store.Process, c.process)
}

func (c *Container) storeMounts() error {
	return c.store.Store(store.Mounts, c.mounts)
}

func (c *Container) storeDevices() error {
	return c.store.Store(store.DeviceIDs, c.devices)
}

func (c *Container) storeState() error {
	return c.store.Store(store.State, c.state)
}

func (c *Container) loadMounts() ([]Mount, error) {
	var mounts []Mount
	if err := c.store.Load(store.Mounts, &mounts); err != nil {
		return []Mount{}, err
	}

	return mounts, nil
}

func (c *Container) loadDevices() ([]ContainerDevice, error) {
	var devices []ContainerDevice

	if err := c.store.Load(store.DeviceIDs, &devices); err != nil {
		return []ContainerDevice{}, err
	}

	return devices, nil
}

// setContainerState sets both the in-memory and on-disk state of the
// container.
func (c *Container) setContainerState(state types.StateString) error {
	if state == "" {
		return vcTypes.ErrNeedState
	}

	c.Logger().Debugf("Setting container state from %v to %v", c.state.State, state)
	// update in-memory state
	c.state.State = state

	if c.sandbox.supportNewStore() {
		// flush data to storage
		if err := c.sandbox.Save(); err != nil {
			return err
		}
	} else {
		// experimental runtime use "persist.json" which doesn't need "state.json" anymore
		// update on-disk state
		if err := c.store.Store(store.State, c.state); err != nil {
			return err
		}
	}

	return nil
}

func (c *Container) shareFiles(m Mount, idx int, hostSharedDir, guestSharedDir string) (string, bool, error) {
	randBytes, err := utils.GenerateRandomBytes(8)
	if err != nil {
		return "", false, err
	}

	filename := fmt.Sprintf("%s-%s-%s", c.id, hex.EncodeToString(randBytes), filepath.Base(m.Destination))
	guestDest := filepath.Join(guestSharedDir, filename)

	// copy file to contaier's rootfs if filesystem sharing is not supported, otherwise
	// bind mount it in the shared directory.
	caps := c.sandbox.hypervisor.capabilities()
	if !caps.IsFsSharingSupported() {
		c.Logger().Debug("filesystem sharing is not supported, files will be copied")

		fileInfo, err := os.Stat(m.Source)
		if err != nil {
			return "", false, err
		}

		// Ignore the mount if this is not a regular file (excludes
		// directory, socket, device, ...) as it cannot be handled by
		// a simple copy. But this should not be treated as an error,
		// only as a limitation.
		if !fileInfo.Mode().IsRegular() {
			c.Logger().WithField("ignored-file", m.Source).Debug("Ignoring non-regular file as FS sharing not supported")
			return "", true, nil
		}

		if err := c.sandbox.agent.copyFile(m.Source, guestDest); err != nil {
			return "", false, err
		}
	} else {
		// These mounts are created in the shared dir
		mountDest := filepath.Join(hostSharedDir, c.sandbox.id, filename)
		if err := bindMount(c.ctx, m.Source, mountDest, false); err != nil {
			return "", false, err
		}
		// Save HostPath mount value into the mount list of the container.
		c.mounts[idx].HostPath = mountDest
	}

	return guestDest, false, nil
}

// mountSharedDirMounts handles bind-mounts by bindmounting to the host shared
// directory which is mounted through 9pfs in the VM.
// It also updates the container mount list with the HostPath info, and store
// container mounts to the storage. This way, we will have the HostPath info
// available when we will need to unmount those mounts.
func (c *Container) mountSharedDirMounts(hostSharedDir, guestSharedDir string) ([]Mount, []Mount, error) {
	var sharedDirMounts []Mount
	var ignoredMounts []Mount
	for idx, m := range c.mounts {
		// Skip mounting certain system paths from the source on the host side
		// into the container as it does not make sense to do so.
		// Example sources could be /sys/fs/cgroup etc.
		if isSystemMount(m.Source) {
			continue
		}

		if m.Type != "bind" {
			continue
		}

		// We need to treat /dev/shm as a special case. This is passed as a bind mount in the spec,
		// but it does not make sense to pass this as a 9p mount from the host side.
		// This needs to be handled purely in the guest, by allocating memory for this inside the VM.
		if m.Destination == "/dev/shm" {
			continue
		}

		// Check if mount is a block device file. If it is, the block device will be attached to the host
		// instead of passing this as a shared mount.
		if len(m.BlockDeviceID) > 0 {
			// Attach this block device, all other devices passed in the config have been attached at this point
			if err := c.sandbox.devManager.AttachDevice(m.BlockDeviceID, c.sandbox); err != nil {
				return nil, nil, err
			}

			if !c.sandbox.supportNewStore() {
				if err := c.sandbox.storeSandboxDevices(); err != nil {
					//TODO: roll back?
					return nil, nil, err
				}
			}
			continue
		}

		// Ignore /dev, directories and all other device files. We handle
		// only regular files in /dev. It does not make sense to pass the host
		// device nodes to the guest.
		if isHostDevice(m.Destination) {
			continue
		}

		guestDest, ignore, err := c.shareFiles(m, idx, hostSharedDir, guestSharedDir)
		if err != nil {
			return nil, nil, err
		}

		// Expand the list of mounts to ignore.
		if ignore {
			ignoredMounts = append(ignoredMounts, Mount{Source: m.Source})
			continue
		}

		// Check if mount is readonly, let the agent handle the readonly mount
		// within the VM.
		readonly := false
		for _, flag := range m.Options {
			if flag == "ro" {
				readonly = true
			}
		}

		sharedDirMount := Mount{
			Source:      guestDest,
			Destination: m.Destination,
			Type:        m.Type,
			Options:     m.Options,
			ReadOnly:    readonly,
		}

		sharedDirMounts = append(sharedDirMounts, sharedDirMount)
	}

	if !c.sandbox.supportNewStore() {
		if err := c.storeMounts(); err != nil {
			return nil, nil, err
		}
	}

	return sharedDirMounts, ignoredMounts, nil
}

func (c *Container) unmountHostMounts() error {
	var span opentracing.Span
	span, c.ctx = c.trace("unmountHostMounts")
	defer span.Finish()

	for _, m := range c.mounts {
		if m.HostPath != "" {
			span, _ := c.trace("unmount")
			span.SetTag("host-path", m.HostPath)

			if err := syscall.Unmount(m.HostPath, syscall.MNT_DETACH); err != nil {
				c.Logger().WithFields(logrus.Fields{
					"host-path": m.HostPath,
					"error":     err,
				}).Warn("Could not umount")
				return err
			}

			if m.Type == "bind" {
				s, err := os.Stat(m.HostPath)
				if err != nil {
					return errors.Wrapf(err, "Could not stat host-path %v", m.HostPath)
				}
				// Remove the empty file or directory
				if s.Mode().IsRegular() && s.Size() == 0 {
					os.Remove(m.HostPath)
				}
				if s.Mode().IsDir() {
					syscall.Rmdir(m.HostPath)
				}
			}

			span.Finish()
		}
	}

	return nil
}

func filterDevices(c *Container, devices []ContainerDevice) (ret []ContainerDevice) {
	for _, dev := range devices {
		major, _ := c.sandbox.devManager.GetDeviceByID(dev.ID).GetMajorMinor()
		if _, ok := cdromMajors[major]; ok {
			c.Logger().WithFields(logrus.Fields{
				"device": dev.ContainerPath,
			}).Info("Not attach device because it is a CDROM")
			continue
		}

		if major == floppyMajor {
			c.Logger().WithFields(logrus.Fields{
				"device": dev.ContainerPath,
			}).Info("Not attaching device because it is a floppy drive")
			continue
		}

		ret = append(ret, dev)
	}
	return
}

func (c *Container) createBlockDevices() error {
	// iterate all mounts and create block device if it's block based.
	for i, m := range c.mounts {
		if len(m.BlockDeviceID) > 0 || m.Type != "bind" {
			// Non-empty m.BlockDeviceID indicates there's already one device
			// associated with the mount,so no need to create a new device for it
			// and we only create block device for bind mount
			continue
		}

		var stat unix.Stat_t
		if err := unix.Stat(m.Source, &stat); err != nil {
			return fmt.Errorf("stat %q failed: %v", m.Source, err)
		}

		// Check if mount is a block device file. If it is, the block device will be attached to the host
		// instead of passing this as a shared mount.
		if c.checkBlockDeviceSupport() && stat.Mode&unix.S_IFBLK == unix.S_IFBLK {
			b, err := c.sandbox.devManager.NewDevice(config.DeviceInfo{
				HostPath:      m.Source,
				ContainerPath: m.Destination,
				DevType:       "b",
				Major:         int64(unix.Major(stat.Rdev)),
				Minor:         int64(unix.Minor(stat.Rdev)),
			})
			if err != nil {
				return fmt.Errorf("device manager failed to create new device for %q: %v", m.Source, err)
			}

			c.mounts[i].BlockDeviceID = b.DeviceID()
		}
	}

	return nil
}

// newContainer creates a Container structure from a sandbox and a container configuration.
func newContainer(sandbox *Sandbox, contConfig ContainerConfig) (*Container, error) {
	span, _ := sandbox.trace("newContainer")
	defer span.Finish()

	if !contConfig.valid() {
		return &Container{}, fmt.Errorf("Invalid container configuration")
	}

	c := &Container{
		id:            contConfig.ID,
		sandboxID:     sandbox.id,
		rootFs:        contConfig.RootFs,
		config:        &contConfig,
		sandbox:       sandbox,
		runPath:       store.ContainerRuntimeRootPath(sandbox.id, contConfig.ID),
		configPath:    store.ContainerConfigurationRootPath(sandbox.id, contConfig.ID),
		containerPath: filepath.Join(sandbox.id, contConfig.ID),
		rootfsSuffix:  "rootfs",
		state:         types.ContainerState{},
		process:       Process{},
		mounts:        contConfig.Mounts,
		ctx:           sandbox.ctx,
	}

	storeAlreadyExists := store.VCContainerStoreExists(sandbox.ctx, c.sandboxID, c.id)
	ctrStore, err := store.NewVCContainerStore(sandbox.ctx, c.sandboxID, c.id)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil && !storeAlreadyExists {
			if delerr := c.store.Delete(); delerr != nil {
				c.Logger().WithError(delerr).WithField("cid", c.id).Error("delete store failed")
			}
		}
	}()

	c.store = ctrStore

	// experimental runtime use "persist.json" instead of legacy "state.json" as storage
	if c.sandbox.supportNewStore() {
		err := c.Restore()
		if err == nil {
			//container restored
			return c, nil
		}

		// Unexpected error
		if !os.IsNotExist(err) && err != errContainerPersistNotExist {
			return nil, err
		}
		// Go to next step for first created container
	} else {
		state, err := c.store.LoadContainerState()
		if err == nil {
			c.state = state
		}

		var process Process
		if err := c.store.Load(store.Process, &process); err == nil {
			c.process = process
		}
	}

	if err = c.createMounts(); err != nil {
		return nil, err
	}

	if err = c.createDevices(contConfig); err != nil {
		return nil, err
	}

	return c, nil
}

func (c *Container) createMounts() error {
	// If sandbox supports "newstore", only newly created container can reach this function,
	// so we don't call restore when `supportNewStore` is true
	if !c.sandbox.supportNewStore() {
		mounts, err := c.loadMounts()
		if err == nil {
			// restore mounts from disk
			c.mounts = mounts
			return nil
		}
	}

	// Create block devices for newly created container
	if err := c.createBlockDevices(); err != nil {
		return err
	}

	return nil
}

func (c *Container) createDevices(contConfig ContainerConfig) error {
	// If sandbox supports "newstore", only newly created container can reach this function,
	// so we don't call restore when `supportNewStore` is true
	if !c.sandbox.supportNewStore() {
		// Devices will be found in storage after create stage has completed.
		// We load devices from storage at all other stages.
		storedDevices, err := c.loadDevices()
		if err == nil {
			c.devices = storedDevices
			return nil
		}
	}

	// If devices were not found in storage, create Device implementations
	// from the configuration. This should happen at create.
	var storedDevices []ContainerDevice
	for _, info := range contConfig.DeviceInfos {
		dev, err := c.sandbox.devManager.NewDevice(info)
		if err != nil {
			return err
		}

		storedDevices = append(storedDevices, ContainerDevice{
			ID:            dev.DeviceID(),
			ContainerPath: info.ContainerPath,
			FileMode:      info.FileMode,
			UID:           info.UID,
			GID:           info.GID,
		})
	}
	c.devices = filterDevices(c, storedDevices)
	return nil
}

// rollbackFailingContainerCreation rolls back important steps that might have
// been performed before the container creation failed.
// - Unplug CPU and memory resources from the VM.
// - Unplug devices from the VM.
func (c *Container) rollbackFailingContainerCreation() {
	if err := c.detachDevices(); err != nil {
		c.Logger().WithError(err).Error("rollback failed detachDevices()")
	}
	if err := c.removeDrive(); err != nil {
		c.Logger().WithError(err).Error("rollback failed removeDrive()")
	}
}

func (c *Container) checkBlockDeviceSupport() bool {
	if !c.sandbox.config.HypervisorConfig.DisableBlockDeviceUse {
		agentCaps := c.sandbox.agent.capabilities()
		hypervisorCaps := c.sandbox.hypervisor.capabilities()

		if agentCaps.IsBlockDeviceSupported() && hypervisorCaps.IsBlockDeviceHotplugSupported() {
			return true
		}
	}

	return false
}

// createContainer creates and start a container inside a Sandbox. It has to be
// called only when a new container, not known by the sandbox, has to be created.
func (c *Container) create() (err error) {
	// In case the container creation fails, the following takes care
	// of rolling back all the actions previously performed.
	defer func() {
		if err != nil {
			c.rollbackFailingContainerCreation()
		}
	}()

	if c.checkBlockDeviceSupport() {
		if err = c.hotplugDrive(); err != nil {
			return
		}
	}

	// Attach devices
	if err = c.attachDevices(); err != nil {
		return
	}

	// Deduce additional system mount info that should be handled by the agent
	// inside the VM
	c.getSystemMountInfo()

	if !c.sandbox.supportNewStore() {
		if err = c.storeDevices(); err != nil {
			return
		}
	}

	process, err := c.sandbox.agent.createContainer(c.sandbox, c)
	if err != nil {
		return err
	}
	c.process = *process

	if !c.sandbox.config.SandboxCgroupOnly {
		if err = c.cgroupsCreate(); err != nil {
			return
		}
	}

	if !c.sandbox.supportNewStore() {
		// Store the container process returned by the agent.
		if err = c.storeProcess(); err != nil {
			return
		}
	}

	if err = c.setContainerState(types.StateReady); err != nil {
		return
	}

	return nil
}

func (c *Container) delete() error {
	if c.state.State != types.StateReady &&
		c.state.State != types.StateStopped {
		return fmt.Errorf("Container not ready or stopped, impossible to delete")
	}

	// Remove the container from sandbox structure
	if err := c.sandbox.removeContainer(c.id); err != nil {
		return err
	}

	if !c.sandbox.config.SandboxCgroupOnly {
		if err := c.cgroupsDelete(); err != nil {
			return err
		}
	}

	return c.store.Delete()
}

// checkSandboxRunning validates the container state.
//
// cmd specifies the operation (or verb) that the retrieval is destined
// for and is only used to make the returned error as descriptive as
// possible.
func (c *Container) checkSandboxRunning(cmd string) error {
	if cmd == "" {
		return fmt.Errorf("Cmd cannot be empty")
	}

	if c.sandbox.state.State != types.StateRunning {
		return fmt.Errorf("Sandbox not running, impossible to %s the container", cmd)
	}

	return nil
}

func (c *Container) getSystemMountInfo() {
	// check if /dev needs to be bind mounted from host /dev
	c.systemMountsInfo.BindMountDev = false

	for _, m := range c.mounts {
		if m.Source == "/dev" && m.Destination == "/dev" && m.Type == "bind" {
			c.systemMountsInfo.BindMountDev = true
		}
	}

	// TODO Deduce /dev/shm size. See https://github.com/clearcontainers/runtime/issues/138
}

func (c *Container) start() error {
	if err := c.checkSandboxRunning("start"); err != nil {
		return err
	}

	if c.state.State != types.StateReady &&
		c.state.State != types.StateStopped {
		return fmt.Errorf("Container not ready or stopped, impossible to start")
	}

	if err := c.state.ValidTransition(c.state.State, types.StateRunning); err != nil {
		return err
	}

	if err := c.sandbox.agent.startContainer(c.sandbox, c); err != nil {
		c.Logger().WithError(err).Error("Failed to start container")

		if err := c.stop(true); err != nil {
			c.Logger().WithError(err).Warn("Failed to stop container")
		}
		return err
	}

	return c.setContainerState(types.StateRunning)
}

func (c *Container) stop(force bool) error {
	span, _ := c.trace("stop")
	defer span.Finish()

	// In case the container status has been updated implicitly because
	// the container process has terminated, it might be possible that
	// someone try to stop the container, and we don't want to issue an
	// error in that case. This should be a no-op.
	//
	// This has to be handled before the transition validation since this
	// is an exception.
	if c.state.State == types.StateStopped {
		c.Logger().Info("Container already stopped")
		return nil
	}

	if err := c.state.ValidTransition(c.state.State, types.StateStopped); err != nil {
		return err
	}

	defer func() {
		span, _ := c.trace("stopShim")
		defer span.Finish()

		// If shim is still running something went wrong
		// Make sure we stop the shim process
		if running, _ := isShimRunning(c.process.Pid); running {
			l := c.Logger()
			l.Error("Failed to stop container so stopping dangling shim")
			if err := stopShim(c.process.Pid); err != nil {
				l.WithError(err).Warn("failed to stop shim")
			}
		}

	}()

	// Here we expect that stop() has been called because the container
	// process returned or because it received a signal. In case of a
	// signal, we want to give it some time to end the container process.
	// However, if the signal didn't reach its goal, the caller still
	// expects this container to be stopped, that's why we should not
	// return an error, but instead try to kill it forcefully.
	if err := waitForShim(c.process.Pid); err != nil {
		// Force the container to be killed.
		if err := c.kill(syscall.SIGKILL, true); err != nil && !force {
			return err
		}

		// Wait for the end of container process. We expect this call
		// to succeed. Indeed, we have already given a second chance
		// to the container by trying to kill it with SIGKILL, there
		// is no reason to try to go further if we got an error.
		if err := waitForShim(c.process.Pid); err != nil && !force {
			return err
		}
	}

	// Force the container to be killed. For most of the cases, this
	// should not matter and it should return an error that will be
	// ignored.
	// But for the specific case where the shim has been SIGKILL'ed,
	// the container is still running inside the VM. And this is why
	// this signal will ensure the container will get killed to match
	// the state of the shim. This will allow the following call to
	// stopContainer() to succeed in such particular case.
	c.kill(syscall.SIGKILL, true)

	// Since the agent has supported the MultiWaitProcess, it's better to
	// wait the process here to make sure the process has exited before to
	// issue stopContainer, otherwise the RemoveContainerRequest in it will
	// get failed if the process hasn't exited.
	c.sandbox.agent.waitProcess(c, c.id)

	// container was killed by force, container MUST change its state
	// as soon as possible just in case one of below operations fail leaving
	// the containers in a bad state.
	if err := c.setContainerState(types.StateStopped); err != nil {
		return err
	}

	defer func() {
		// Save device and drive data.
		// TODO: can we merge this saving with setContainerState()?
		if c.sandbox.supportNewStore() {
			if err := c.sandbox.Save(); err != nil {
				c.Logger().WithError(err).Info("save container state failed")
			}
		}
	}()

	if err := c.sandbox.agent.stopContainer(c.sandbox, *c); err != nil && !force {
		return err
	}

	if err := c.unmountHostMounts(); err != nil && !force {
		return err
	}

	if err := bindUnmountContainerRootfs(c.ctx, kataHostSharedDir, c.sandbox.id, c.id); err != nil && !force {
		return err
	}

	if err := c.detachDevices(); err != nil && !force {
		return err
	}

	if err := c.removeDrive(); err != nil && !force {
		return err
	}

	return nil
}

func (c *Container) enter(cmd types.Cmd) (*Process, error) {
	if err := c.checkSandboxRunning("enter"); err != nil {
		return nil, err
	}

	if c.state.State != types.StateReady &&
		c.state.State != types.StateRunning {
		return nil, fmt.Errorf("Container not ready or running, " +
			"impossible to enter")
	}

	process, err := c.sandbox.agent.exec(c.sandbox, *c, cmd)
	if err != nil {
		return nil, err
	}

	return process, nil
}

func (c *Container) wait(processID string) (int32, error) {
	if c.state.State != types.StateReady &&
		c.state.State != types.StateRunning {
		return 0, fmt.Errorf("Container not ready or running, " +
			"impossible to wait")
	}

	return c.sandbox.agent.waitProcess(c, processID)
}

func (c *Container) kill(signal syscall.Signal, all bool) error {
	return c.signalProcess(c.process.Token, signal, all)
}

func (c *Container) signalProcess(processID string, signal syscall.Signal, all bool) error {
	if c.sandbox.state.State != types.StateReady && c.sandbox.state.State != types.StateRunning {
		return fmt.Errorf("Sandbox not ready or running, impossible to signal the container")
	}

	if c.state.State != types.StateReady && c.state.State != types.StateRunning && c.state.State != types.StatePaused {
		return fmt.Errorf("Container not ready, running or paused, impossible to signal the container")
	}

	return c.sandbox.agent.signalProcess(c, processID, signal, all)
}

func (c *Container) winsizeProcess(processID string, height, width uint32) error {
	if c.state.State != types.StateReady && c.state.State != types.StateRunning {
		return fmt.Errorf("Container not ready or running, impossible to signal the container")
	}

	return c.sandbox.agent.winsizeProcess(c, processID, height, width)
}

func (c *Container) ioStream(processID string) (io.WriteCloser, io.Reader, io.Reader, error) {
	if c.state.State != types.StateReady && c.state.State != types.StateRunning {
		return nil, nil, nil, fmt.Errorf("Container not ready or running, impossible to signal the container")
	}

	stream := newIOStream(c.sandbox, c, processID)

	return stream.stdin(), stream.stdout(), stream.stderr(), nil
}

func (c *Container) processList(options ProcessListOptions) (ProcessList, error) {
	if err := c.checkSandboxRunning("ps"); err != nil {
		return nil, err
	}

	if c.state.State != types.StateRunning {
		return nil, fmt.Errorf("Container not running, impossible to list processes")
	}

	return c.sandbox.agent.processListContainer(c.sandbox, *c, options)
}

func (c *Container) stats() (*ContainerStats, error) {
	if err := c.checkSandboxRunning("stats"); err != nil {
		return nil, err
	}
	return c.sandbox.agent.statsContainer(c.sandbox, *c)
}

func (c *Container) update(resources specs.LinuxResources) error {
	if err := c.checkSandboxRunning("update"); err != nil {
		return err
	}

	if state := c.state.State; !(state == types.StateRunning || state == types.StateReady) {
		return fmt.Errorf("Container(%s) not running or ready, impossible to update", state)
	}

	if c.config.Resources.CPU == nil {
		c.config.Resources.CPU = &specs.LinuxCPU{}
	}

	if cpu := resources.CPU; cpu != nil {
		if p := cpu.Period; p != nil && *p != 0 {
			c.config.Resources.CPU.Period = p
		}
		if q := cpu.Quota; q != nil && *q != 0 {
			c.config.Resources.CPU.Quota = q
		}
	}

	if c.config.Resources.Memory == nil {
		c.config.Resources.Memory = &specs.LinuxMemory{}
	}

	if mem := resources.Memory; mem != nil && mem.Limit != nil {
		c.config.Resources.Memory.Limit = mem.Limit
	}

	if err := c.sandbox.updateResources(); err != nil {
		return err
	}

	if !c.sandbox.config.SandboxCgroupOnly {
		if err := c.cgroupsUpdate(resources); err != nil {
			return err
		}
	}

	return c.sandbox.agent.updateContainer(c.sandbox, *c, resources)
}

func (c *Container) pause() error {
	if err := c.checkSandboxRunning("pause"); err != nil {
		return err
	}

	if c.state.State != types.StateRunning {
		return fmt.Errorf("Container not running, impossible to pause")
	}

	if err := c.sandbox.agent.pauseContainer(c.sandbox, *c); err != nil {
		return err
	}

	return c.setContainerState(types.StatePaused)
}

func (c *Container) resume() error {
	if err := c.checkSandboxRunning("resume"); err != nil {
		return err
	}

	if c.state.State != types.StatePaused {
		return fmt.Errorf("Container not paused, impossible to resume")
	}

	if err := c.sandbox.agent.resumeContainer(c.sandbox, *c); err != nil {
		return err
	}

	return c.setContainerState(types.StateRunning)
}

func (c *Container) hotplugDrive() error {
	var dev device
	var err error

	// container rootfs is blockdevice backed and isn't mounted
	if !c.rootFs.Mounted {
		dev, err = getDeviceForPath(c.rootFs.Source)
		// there is no "rootfs" dir on block device backed rootfs
		c.rootfsSuffix = ""
	} else {
		dev, err = getDeviceForPath(c.rootFs.Target)
	}

	if err == errMountPointNotFound {
		return nil
	}

	if err != nil {
		return err
	}

	c.Logger().WithFields(logrus.Fields{
		"device-major": dev.major,
		"device-minor": dev.minor,
		"mount-point":  dev.mountPoint,
	}).Info("device details")

	isDM, err := checkStorageDriver(dev.major, dev.minor)
	if err != nil {
		return err
	}

	if !isDM {
		return nil
	}

	devicePath := c.rootFs.Source
	fsType := c.rootFs.Type
	if c.rootFs.Mounted {
		if dev.mountPoint == c.rootFs.Target {
			c.rootfsSuffix = ""
		}
		// If device mapper device, then fetch the full path of the device
		devicePath, fsType, err = GetDevicePathAndFsType(dev.mountPoint)
		if err != nil {
			return err
		}
	}

	devicePath, err = filepath.EvalSymlinks(devicePath)
	if err != nil {
		return err
	}

	c.Logger().WithFields(logrus.Fields{
		"device-path": devicePath,
		"fs-type":     fsType,
	}).Info("Block device detected")

	if err = c.plugDevice(devicePath); err != nil {
		return err
	}

	return c.setStateFstype(fsType)
}

func (c *Container) plugDevice(devicePath string) error {
	var stat unix.Stat_t
	if err := unix.Stat(devicePath, &stat); err != nil {
		return fmt.Errorf("stat %q failed: %v", devicePath, err)
	}

	if c.checkBlockDeviceSupport() && stat.Mode&unix.S_IFBLK == unix.S_IFBLK {
		b, err := c.sandbox.devManager.NewDevice(config.DeviceInfo{
			HostPath:      devicePath,
			ContainerPath: filepath.Join(kataGuestSharedDir, c.id),
			DevType:       "b",
			Major:         int64(unix.Major(stat.Rdev)),
			Minor:         int64(unix.Minor(stat.Rdev)),
		})
		if err != nil {
			return fmt.Errorf("device manager failed to create rootfs device for %q: %v", devicePath, err)
		}

		c.state.BlockDeviceID = b.DeviceID()

		// attach rootfs device
		if err := c.sandbox.devManager.AttachDevice(b.DeviceID(), c.sandbox); err != nil {
			return err
		}

		if !c.sandbox.supportNewStore() {
			if err := c.sandbox.storeSandboxDevices(); err != nil {
				return err
			}
		}
	}
	return nil
}

// isDriveUsed checks if a drive has been used for container rootfs
func (c *Container) isDriveUsed() bool {
	return !(c.state.Fstype == "")
}

func (c *Container) removeDrive() (err error) {
	if c.isDriveUsed() {
		c.Logger().Info("unplugging block device")

		devID := c.state.BlockDeviceID
		err := c.sandbox.devManager.DetachDevice(devID, c.sandbox)
		if err != nil && err != manager.ErrDeviceNotAttached {
			return err
		}

		if err = c.sandbox.devManager.RemoveDevice(devID); err != nil {
			c.Logger().WithFields(logrus.Fields{
				"container": c.id,
				"device-id": devID,
			}).WithError(err).Error("remove device failed")

			// ignore the device not exist error
			if err != manager.ErrDeviceNotExist {
				return err
			}
		}

		if !c.sandbox.supportNewStore() {
			if err := c.sandbox.storeSandboxDevices(); err != nil {
				return err
			}
		}
	}

	return nil
}

func (c *Container) attachDevices() error {
	// there's no need to do rollback when error happens,
	// because if attachDevices fails, container creation will fail too,
	// and rollbackFailingContainerCreation could do all the rollbacks
	for _, dev := range c.devices {
		if err := c.sandbox.devManager.AttachDevice(dev.ID, c.sandbox); err != nil {
			return err
		}
	}

	if !c.sandbox.supportNewStore() {
		if err := c.sandbox.storeSandboxDevices(); err != nil {
			return err
		}
	}
	return nil
}

func (c *Container) detachDevices() error {
	for _, dev := range c.devices {
		err := c.sandbox.devManager.DetachDevice(dev.ID, c.sandbox)
		if err != nil && err != manager.ErrDeviceNotAttached {
			return err
		}

		if err = c.sandbox.devManager.RemoveDevice(dev.ID); err != nil {
			c.Logger().WithFields(logrus.Fields{
				"container": c.id,
				"device-id": dev.ID,
			}).WithError(err).Error("remove device failed")

			// ignore the device not exist error
			if err != manager.ErrDeviceNotExist {
				return err
			}
		}
	}

	if !c.sandbox.supportNewStore() {
		if err := c.sandbox.storeSandboxDevices(); err != nil {
			return err
		}
	}
	return nil
}

// cgroupsCreate creates cgroups on the host for the associated container
func (c *Container) cgroupsCreate() (err error) {
	spec := c.GetOCISpec()
	if spec == nil {
		return errorMissingOCISpec
	}

	// https://github.com/kata-containers/runtime/issues/168
	resources := specs.LinuxResources{
		CPU: nil,
	}

	if spec.Linux != nil && spec.Linux.Resources != nil {
		resources.CPU = validCPUResources(spec.Linux.Resources.CPU)
	}

	cgroupPath := utils.ValidCgroupPath(spec.Linux.CgroupsPath)
	c.state.CgroupPath, err = renameCgroupPath(cgroupPath)
	if err != nil {
		return err
	}

	cgroup, err := cgroupsNewFunc(cgroups.V1,
		cgroups.StaticPath(c.state.CgroupPath), &resources)
	if err != nil {
		return fmt.Errorf("Could not create cgroup for %v: %v", c.state.CgroupPath, err)
	}

	c.config.Resources = resources

	// Add shim into cgroup
	if c.process.Pid > 0 {
		if err := cgroup.Add(cgroups.Process{Pid: c.process.Pid}); err != nil {
			return fmt.Errorf("Could not add PID %d to cgroup %v: %v", c.process.Pid, spec.Linux.CgroupsPath, err)
		}
	}

	return nil
}

// cgroupsDelete deletes the cgroups on the host for the associated container
func (c *Container) cgroupsDelete() error {

	if c.state.CgroupPath == "" {
		c.Logger().Debug("container does not have host cgroups: nothing to update")
		return nil
	}

	cgroup, err := cgroupsLoadFunc(cgroups.V1,
		cgroups.StaticPath(c.state.CgroupPath))

	if err == cgroups.ErrCgroupDeleted {
		// cgroup already deleted
		return nil
	}

	if err != nil {
		return fmt.Errorf("Could not load container cgroup %v: %v", c.state.CgroupPath, err)
	}

	// move running process here, that way cgroup can be removed
	parent, err := parentCgroup(cgroups.V1, c.state.CgroupPath)
	if err != nil {
		// parent cgroup doesn't exist, that means there are no process running
		// and the container cgroup was removed.
		c.Logger().WithError(err).Warn("Container cgroup doesn't exist")
		return nil
	}

	if err := cgroup.MoveTo(parent); err != nil {
		// Don't fail, cgroup can be deleted
		c.Logger().WithError(err).Warn("Could not move container process into parent cgroup")
	}

	if err := cgroup.Delete(); err != nil {
		return fmt.Errorf("Could not delete container cgroup path='%v': error='%v'", c.state.CgroupPath, err)
	}

	return nil
}

// cgroupsUpdate updates cgroups on the host for the associated container
func (c *Container) cgroupsUpdate(resources specs.LinuxResources) error {

	if c.state.CgroupPath == "" {
		c.Logger().Debug("container does not have host cgroups: nothing to update")
		return nil
	}
	cgroup, err := cgroupsLoadFunc(cgroups.V1,
		cgroups.StaticPath(c.state.CgroupPath))
	if err != nil {
		return fmt.Errorf("Could not load cgroup %v: %v", c.state.CgroupPath, err)
	}

	// Issue: https://github.com/kata-containers/runtime/issues/168
	r := specs.LinuxResources{
		CPU: validCPUResources(resources.CPU),
	}

	// update cgroup
	if err := cgroup.Update(&r); err != nil {
		return fmt.Errorf("Could not update container cgroup path='%v': error='%v'", c.state.CgroupPath, err)
	}

	// store new resources
	c.config.Resources = r
	if err := c.storeContainer(); err != nil {
		return err
	}

	return nil
}
