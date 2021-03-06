package utils

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/context"

	"github.com/Sirupsen/logrus"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	apis "github.com/openebs/csi/pkg/apis/openebs.io/core/v1alpha1"
	service "github.com/openebs/csi/pkg/generated/maya/kubernetes/service/v1alpha1"
	iscsi "github.com/openebs/csi/pkg/iscsi/v1alpha1"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/util/mount"
)

const (
	// TODO make VolumeWaitTimeout as env
	VolumeWaitTimeout = 2

	// TODO make VolumeWaitRetryCount as env
	VolumeWaitRetryCount = 6

	// TODO make MonitorMountRetryTimeout as env
	MonitorMountRetryTimeout = 5
)

var (
	// MAPIServerEndpoint is the address to connect
	// to maya apiserver
	MAPIServerEndpoint string

	// OpenEBSNamespace is openebs system namespace
	OpenEBSNamespace string

	// Volumes contains the list of volumes created in case of controller plugin
	// and list of volumes attached to this node in node plugin
	// This list is protected by VolumesListLock
	Volumes map[string]*apis.CSIVolume

	// VolumesListLock is required to protect the above Volumes list
	VolumesListLock sync.RWMutex

	// ReqMountList contains the list of volumes which are required
	// to be remounted. This list is secured by ReqMountListLock
	ReqMountList map[string]bool

	// ReqMountListLock is required to protect the above ReqMount list
	ReqMountListLock sync.RWMutex
)

const (
	// timmeout indiactes the REST call timeouts
	timeout = 60 * time.Second
)

func init() {

	OpenEBSNamespace = os.Getenv("OPENEBS_NAMESPACE")
	if OpenEBSNamespace == "" {
		logrus.Fatalf("OPENEBS_NAMESPACE environment variable not set")
	}

	// Get MAPI_ServiceName from env OPENEBS_MAPI_SVC
	// If this env is not set its better to crash the container since its a
	// configuration problem. Without this service its not possible to create
	// or delete volumes
	MAPIServiceName := os.Getenv("OPENEBS_MAPI_SVC")
	if MAPIServiceName == "" {
		logrus.Fatalf("OPENEBS_MAPI_SVC environment variable not set")
	}

	svc, err := service.NewKubeclient().
		WithNamespace(OpenEBSNamespace).
		Get(MAPIServiceName, metav1.GetOptions{})
	if err != nil {
		// If error occurs over here then there are 2 possibilities either the
		// service was not created or KubeAPIServer is not reachable
		// In both the cases the driver cannot be started
		logrus.Fatalf(err.Error())
	}

	svcIP := svc.Spec.ClusterIP
	svcPort := strconv.FormatInt(int64(svc.Spec.Ports[0].Port), 10)
	MAPIServerEndpoint = "http://" + svcIP + ":" + svcPort

	Volumes = map[string]*apis.CSIVolume{}
	ReqMountList = make(map[string]bool)

}

// parseEndpoint should have a valid prefix(unix/tcp) to return a valid endpoint parts
func parseEndpoint(ep string) (string, string, error) {
	if strings.HasPrefix(strings.ToLower(ep), "unix://") || strings.HasPrefix(strings.ToLower(ep), "tcp://") {
		s := strings.SplitN(ep, "://", 2)
		if s[1] != "" {
			return s[0], s[1], nil
		}
	}
	return "", "", fmt.Errorf("Invalid endpoint: %v", ep)
}

// logGRPC logs all the grpc related errors, i.e the final errors
// which are returned to the grpc clients
func logGRPC(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	logrus.Infof("GRPC call: %s", info.FullMethod)
	logrus.Infof("GRPC request: %s", protosanitizer.StripSecrets(req))
	resp, err := handler(ctx, req)
	if err != nil {
		logrus.Errorf("GRPC error: %v", err)
	} else {
		logrus.Infof("GRPC response: %s", protosanitizer.StripSecrets(resp))
	}
	return resp, err
}

// ChmodMountPath removes all permission from the folder if volume is not
// mounted on it
func ChmodMountPath(mountPath string) error {
	return os.Chmod(mountPath, 0000)
}

// WaitForVolumeToBeReachable keeps the mounts on hold until the volume is
// reachable
func WaitForVolumeToBeReachable(targetPortal string) error {
	var (
		retries int
		err     error
		conn    net.Conn
	)

	for {
		// Create a connection to test if the iSCSI Portal is reachable,
		if conn, err = net.Dial("tcp", targetPortal); err == nil {
			conn.Close()
			logrus.Infof("Volume is reachable to create connections")
			return nil
		}
		// wait until the iSCSI targetPortal is reachable
		// There is no pointn of triggering iSCSIadm login commands
		// until the portal is reachable
		time.Sleep(VolumeWaitTimeout * time.Second)
		retries++
		if retries >= VolumeWaitRetryCount {
			// Let the caller function decide further if the volume is not reachable
			// even after 12 seconds ( This number was arrived at
			// based on the kubelets retrying logic. Kubelet retries to publish
			// volume after every 14s )
			return fmt.Errorf("iSCSI Target not reachable, TargetPortal %v, err:%v",
				targetPortal, err)
		}
	}
}

// WaitForVolumeToBeReady retrieves the volume info from cstorVolume CR and
// waits until consistency factor is met for connected replicas
func WaitForVolumeToBeReady(volumeID string) error {
	var retries int
checkVolumeStatus:
	// Status is fetched from cstorVolume CR
	volStatus, err := getVolStatus(volumeID)
	if err != nil {
		return err
	} else if volStatus == "Healthy" || volStatus == "Degraded" {
		// In both healthy and degraded states the volume can serve IOs
		logrus.Infof("Volume is ready to accept IOs")
	} else if retries >= VolumeWaitRetryCount {
		// Let the caller function decide further if the volume is still not
		// ready to accdept IOs after 12 seconds ( This number was arrived at
		// based on the kubelets retrying logic. Kubelet retries to publish
		// volume after every 14s )
		return fmt.Errorf("Volume is not ready: Replicas yet to connect to controller")
	} else {
		time.Sleep(VolumeWaitTimeout * time.Second)
		retries++
		goto checkVolumeStatus
	}
	return nil
}

// GetVolumeByName fetches the volume from Volumes list based on th input name
func GetVolumeByName(volName string) (*apis.CSIVolume, error) {
	for _, Vol := range Volumes {
		if Vol.Spec.Volume.Name == volName {
			return Vol, nil
		}
	}
	return nil,
		fmt.Errorf("volume name %s does not exit in the volumes list", volName)
}

// GetVolumeDetails returns a new instance of csiVolume filled with the
// VolumeAttributes fetched from the corresponding PV and some additional info
// required for remounting
func GetVolumeDetails(volumeID, mountPath string, readOnly bool, mountOptions []string) (*apis.CSIVolume, error) {
	pv, err := FetchPVDetails(volumeID)
	if err != nil {
		return nil, err
	}
	vol := apis.CSIVolume{}
	cap := pv.Spec.Capacity[corev1.ResourceName(corev1.ResourceStorage)]
	for _, accessmode := range pv.Spec.AccessModes {
		vol.Spec.Volume.AccessModes = append(vol.Spec.Volume.AccessModes, string(accessmode))
	}
	vol.Spec.Volume.Name = volumeID
	vol.Spec.Volume.FSType = pv.Spec.CSI.FSType
	vol.Spec.Volume.Capacity = cap.String()
	vol.Spec.Volume.MountPath = mountPath
	vol.Spec.Volume.ReadOnly = readOnly
	vol.Spec.Volume.MountOptions = mountOptions
	vol.Spec.ISCSI.Iqn = pv.Spec.CSI.VolumeAttributes["iqn"]
	vol.Spec.ISCSI.Lun = pv.Spec.CSI.VolumeAttributes["lun"]
	vol.Spec.ISCSI.IscsiInterface = pv.Spec.CSI.VolumeAttributes["iscsiInterface"]
	vol.Spec.ISCSI.TargetPortal = pv.Spec.CSI.VolumeAttributes["targetPortal"]
	return &vol, nil
}

func listContains(mountPath string, list []mount.MountPoint) (*mount.MountPoint, bool) {
	for _, info := range list {
		if info.Path == mountPath {
			mntInfo := info
			return &mntInfo, true
		}
	}
	return nil, false
}

// MonitorMounts makes sure that all the volumes present in the inmemory list
// with the driver are mounted with the original mount options
// This function runs a never ending loop therefore should be run as a goroutine
// Mounted list is fetched from the OS and the state of all the volumes is
// reverified after every 5 seconds. If the mountpoint is not present in the
// list or if it has been remounted with a different mount option by the OS, the
// volume is added to the ReqMountList which is removed as soon as the remount
// operation on the volume is complete
// For each remount operation a new goroutine is created, so that if multiple
// volumes have lost their original state they can all be remounted in parallel
func MonitorMounts() {
	mounter := mount.New("")
	ticker := time.NewTicker(MonitorMountRetryTimeout * time.Second)
	for {
		select {
		case <-ticker.C:
			// Get list of mounted paths present with the node
			list, _ := mounter.List()
			VolumesListLock.RLock()
			for _, vol := range Volumes {
				if vol.Spec.Volume.DevicePath == "" {
					// If device path is not set implies the node publish
					// operation is not completed yet
					continue
				}
				// Search the volume in the list of mounted volumes at the node
				// retrieved above
				mountPoint, exists := listContains(vol.Spec.Volume.MountPath, list)
				// If the volume is present in the list verify its state
				if exists && verifyMountOpts(mountPoint.Opts, "rw") {
					// Continue with remaining volumes since this volume looks
					// to be in good shape
					continue
				}
				// Skip remount if the volume is already being remounted
				if _, isRemounting := ReqMountList[vol.Spec.Volume.Name]; isRemounting {
					continue
				}
				// Add volume to the reqMountList and start a goroutine to
				// remount it
				ReqMountListLock.Lock()
				ReqMountList[vol.Spec.Volume.Name] = true
				ReqMountListLock.Unlock()
				go RemountVolume(exists, vol, mountPoint, vol.Spec.Volume.MountPath)
			}
			VolumesListLock.RUnlock()
		}
	}
}

// WaitForVolumeReadyAndReachable waits until the volume is ready to accept IOs
// and is reachable, this function will not come out until both the conditions
// are met. This function stops the driver from overloading the OS with iSCSI
// login commands.
func WaitForVolumeReadyAndReachable(vol *apis.CSIVolume) {
	for {
		// This function return after 12s in case the volume is not ready
		if err := WaitForVolumeToBeReady(vol.Spec.Volume.Name); err != nil {
			logrus.Error(err)
			// Keep retrying until the volume is ready
			continue
		}
		// This function return after 12s in case the volume is not reachable
		if err := WaitForVolumeToBeReachable(vol.Spec.ISCSI.TargetPortal); err == nil {
			return
		} else {
			logrus.Error(err)
		}
	}
}

func verifyMountOpts(opts []string, desiredOpt string) bool {
	for _, opt := range opts {
		if opt == desiredOpt {
			return true
		}
	}
	return false
}

// RemountVolume unmounts the volume if it is already mounted in an undesired
// state and then tries to mount again. If it is not mounted the volume, first
// the disk will be attached via iSCSI login and then it will be mounted
func RemountVolume(exists bool, vol *apis.CSIVolume, mountPoint *mount.MountPoint, desiredMountOpt string) (devicePath string, err error) {
	mounter := mount.New("")
	options := []string{"rw"}
	// Wait until it is possible to chhange the state of mountpoint or when
	// login to volume is possible
	WaitForVolumeReadyAndReachable(vol)
	if exists {
		logrus.Infof("MountPoint:%v IN RO MODE", mountPoint.Path)
		// Unmout and mount operation is performed instead of just remount since
		// the remount option didn't give the desired results
		mounter.Unmount(mountPoint.Path)
		err = mounter.Mount(mountPoint.Device,
			mountPoint.Path, "", options)
	} else {
		// A complete attach and mount is performed if for some reason disk is
		// not present in the mounted list with the OS.
		devicePath, err = iscsi.AttachAndMountDisk(vol)
		//TODO Updadate devicePath in inmemory list and CR
	}
	ReqMountListLock.Lock()
	// Remove the volume from ReqMountList once the remount operation is
	// complete
	delete(ReqMountList, vol.Spec.Volume.Name)
	ReqMountListLock.Unlock()
	return
}
