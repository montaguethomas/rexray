package storage

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"hash"
	"os"
	"strconv"
	"strings"
	"sync"

	gofig "github.com/akutz/gofig/types"
	"github.com/akutz/goof"
	"github.com/rubiojr/go-vhd/vhd"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	autorest "github.com/Azure/go-autorest/autorest/azure"

	"github.com/rexray/rexray/libstorage/api/context"
	"github.com/rexray/rexray/libstorage/api/registry"
	"github.com/rexray/rexray/libstorage/api/types"
	apiUtils "github.com/rexray/rexray/libstorage/api/utils"
	"github.com/rexray/rexray/libstorage/drivers/storage/azureud"
)

const (
	// Name of Blob service in URL
	blobServiceName = "blob"

	// Required by Azure blob name suffix
	vhdExtension = ".vhd"

	// Size 1GB
	size1GB int64 = 1024 * 1024 * 1024

	// Default new disk size
	defaultNewDiskSizeGB int32 = 128

	minSizeGiB = 1
)

type driver struct {
	name             string
	config           gofig.Config
	subscriptionID   string
	resourceGroup    string
	tenantID         string
	storageAccount   string
	storageAccessKey string
	container        string
	clientID         string
	clientSecret     string
	certPath         string
	useHTTPS         bool
}

func init() {
	registry.RegisterStorageDriver(azureud.Name, newDriver)
}

func newDriver() types.StorageDriver {
	return &driver{name: azureud.Name}
}

func (d *driver) Name() string {
	return d.name
}

// Init initializes the driver.
func (d *driver) Init(context types.Context, config gofig.Config) error {
	d.config = config

	d.tenantID = d.getTenantID()
	if d.tenantID == "" {
		return goof.New("tenantID is a required config item")
	}

	d.clientID = d.getClientID()
	if d.clientID == "" {
		return goof.New("clientID is a required config item")
	}

	d.clientSecret = d.getClientSecret()
	d.certPath = d.getCertPath()
	if d.clientSecret == "" && d.certPath == "" {
		return goof.New(
			"clientSecret or certPath must be set for login.")
	}
	if d.clientSecret != "" && d.certPath != "" {
		context.Warn("certPath will be ignored since clientSecret is set")
	}

	d.storageAccount = d.getStorageAccount()
	if d.storageAccount == "" {
		return goof.New("storageAccount is a required config item")
	}

	d.storageAccessKey = d.getStorageAccessKey()
	if d.storageAccessKey == "" {
		return goof.New("storageAccessKey is a required config item")
	}

	d.container = d.getContainer()

	d.subscriptionID = d.getSubscriptionID()
	if d.subscriptionID == "" {
		return goof.New("subscriptionID is a required config item")
	}

	d.resourceGroup = d.getResourceGroup()
	if d.resourceGroup == "" {
		return goof.New("resourceGroup is a required config item")
	}

	d.useHTTPS = d.getUseHTTPS()

	context.Info("storage driver initialized")

	return nil
}

const cacheKeyC = "cacheKey"

type azureSession struct {
	blobStorageClient    *azblob.Client
	computeClientFactory *armcompute.ClientFactory
}

var (
	sessions  = map[string]*azureSession{}
	sessionsL = &sync.Mutex{}
)

func writeHkeyB(h hash.Hash, ps []byte) {
	if ps == nil {
		return
	}
	h.Write(ps)
}

func writeHkey(h hash.Hash, ps *string) {
	writeHkeyB(h, []byte(*ps))
}

var (
	errLoginMsg           = "Failed to login to Azure"
	errAuthFailed         = goof.New(errLoginMsg)
	invalideRsaPrivateKey = goof.New("PKCS#12 certificate must contain an RSA private key")
)

func mustSession(ctx types.Context) *azureSession {
	return context.MustSession(ctx).(*azureSession)
}

func (d *driver) Login(ctx types.Context) (interface{}, error) {
	sessionsL.Lock()
	defer sessionsL.Unlock()

	ctx.Debug("login to azure storage driver")
	var (
		hkey     = md5.New()
		ckey     string
		certData []byte
		cred     azcore.TokenCredential
		err      error
	)

	writeHkey(hkey, &d.subscriptionID)
	writeHkey(hkey, &d.tenantID)
	writeHkey(hkey, &d.storageAccount)
	writeHkey(hkey, &d.clientID)
	ckey = fmt.Sprintf("%x", hkey.Sum(nil))

	if session, ok := sessions[ckey]; ok {
		ctx.WithField(cacheKeyC, ckey).Debug("using cached azure client")
		return session, nil
	}

	if d.clientSecret != "" {
		ctx.Info("Authenticating via clientSecret")
		cred, err = azidentity.NewClientSecretCredential(d.tenantID, d.clientID, d.clientSecret, nil)
		if err != nil {
			return nil, goof.WithError("Failed to create Service Principal Token with client ID and secret", err)
		}
	} else if d.certPath != "" {
		ctx.Info("Authenticating via client certificate")
		certData, err = os.ReadFile(d.certPath)
		if err != nil {
			return nil, goof.WithError("Failed to read provided certificate file", err)
		}

		certificates, rsaPrivateKey, err := azidentity.ParseCertificates(certData, nil)
		if err != nil {
			return nil, goof.WithError("Failed to decode certificate data", err)
		}

		cred, err = azidentity.NewClientCertificateCredential(d.tenantID, d.clientID, certificates, rsaPrivateKey, nil)
		if err != nil {
			return nil, goof.WithError("Failed to create Service Principal Token with certificate", err)
		}
	} else {
		ctx.Info("Authenticating via default credentials")
		cred, err = azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return nil, goof.WithError("Failed to obtain credential", err)
		}
	}

	if _, err := cred.GetToken(ctx, policy.TokenRequestOptions{}); err != nil {
		return nil, goof.WithError("Login does not appear functional", err)
	}

	blobStorageClient, err := azblob.NewClient(fmt.Sprintf("https://%s.blob.core.windows.net/", d.storageAccount), cred, nil)
	if err != nil {
		return nil, goof.WithError("Failed to create BlobStorage client", err)
	}

	computeClientFactory, err := armcompute.NewClientFactory(d.subscriptionID, cred, nil)
	if err != nil {
		return nil, goof.WithError("Failed to create compute client", err)
	}

	session := azureSession{
		blobStorageClient:    blobStorageClient,
		computeClientFactory: computeClientFactory,
	}
	sessions[ckey] = &session

	ctx.WithField(cacheKeyC, ckey).Info(
		"login to azureud storage driver created and cached")

	return &session, nil
}

// NextDeviceInfo returns the information about the driver's next
// available device workflow.
func (d *driver) NextDeviceInfo(
	ctx types.Context) (*types.NextDeviceInfo, error) {
	return nil, nil
}

// Type returns the type of storage the driver provides.
func (d *driver) Type(ctx types.Context) (types.StorageType, error) {
	//Example: Block storage
	return types.Block, nil
}

// InstanceInspect returns an instance.
func (d *driver) InstanceInspect(
	ctx types.Context,
	opts types.Store) (*types.Instance, error) {

	iid := context.MustInstanceID(ctx)
	return &types.Instance{
		InstanceID: iid,
	}, nil
}

// Volumes returns all volumes or a filtered list of volumes.
func (d *driver) Volumes(
	ctx types.Context,
	opts *types.VolumesOpts) ([]*types.Volume, error) {

	pager := mustSession(ctx).blobStorageClient.NewListBlobsFlatPager(d.container,
		&container.ListBlobsFlatOptions{Include: container.ListBlobsInclude{Metadata: true}})

	var blobs []*container.BlobItem
	for pager.More() {
		resp, err := pager.NextPage(ctx)
		if err != nil {
			return nil, goof.WithError("error listing blobs", err)
		}
		blobs = append(blobs, resp.Segment.BlobItems...)
	}

	// Convert retrieved volumes to libStorage types.Volume
	vols, convErr := d.toTypesVolume(ctx, blobs, opts.Attachments)
	if convErr != nil {
		return nil, goof.WithError("error converting to types.Volume", convErr)
	}
	return vols, nil
}

// VolumeInspect inspects a single volume.
func (d *driver) VolumeInspect(
	ctx types.Context,
	volumeID string,
	opts *types.VolumeInspectOpts) (*types.Volume, error) {

	return d.getVolume(ctx, volumeID, opts.Attachments)
}

// VolumeCreate creates a new volume.
func (d *driver) VolumeCreate(ctx types.Context, volumeName string,
	opts *types.VolumeCreateOpts) (*types.Volume, error) {

	if opts.Encrypted != nil && *opts.Encrypted {
		return nil, types.ErrNotImplemented
	}

	if !strings.HasSuffix(volumeName, vhdExtension) {
		ctx.Debugf("Auto-adding %s extension", vhdExtension)
		volumeName = volumeName + vhdExtension
	}

	size := int64(defaultNewDiskSizeGB)
	if opts.Size != nil && *opts.Size >= minSizeGiB {
		size = *opts.Size
	}
	size *= size1GB

	fields := map[string]interface{}{
		"volumeName":  volumeName,
		"sizeInBytes": size,
	}

	err := d.createDiskBlob(ctx, volumeName, size)
	if err != nil {
		return nil, goof.WithFieldsE(fields, "failed to create volume for VM", err)
	}

	return d.getVolume(ctx, volumeName, types.VolAttNone)
}

// VolumeCreateFromSnapshot creates a new volume from an existing snapshot.
func (d *driver) VolumeCreateFromSnapshot(
	ctx types.Context,
	snapshotID, volumeName string,
	opts *types.VolumeCreateOpts) (*types.Volume, error) {
	// TODO Snapshots are not implemented yet
	return nil, types.ErrNotImplemented
}

// VolumeCopy copies an existing volume.
func (d *driver) VolumeCopy(
	ctx types.Context,
	volumeID, volumeName string,
	opts types.Store) (*types.Volume, error) {
	// TODO Snapshots are not implemented yet
	return nil, types.ErrNotImplemented
}

// VolumeSnapshot snapshots a volume.
func (d *driver) VolumeSnapshot(
	ctx types.Context,
	volumeID, snapshotName string,
	opts types.Store) (*types.Snapshot, error) {
	// TODO Snapshots are not implemented yet
	return nil, types.ErrNotImplemented
}

// VolumeRemove removes a volume.
func (d *driver) VolumeRemove(ctx types.Context, volumeID string, opts *types.VolumeRemoveOpts) error {
	//TODO check if volume is attached? if so fail
	_, err := mustSession(ctx).blobStorageClient.DeleteBlob(ctx, d.container, volumeID, nil)
	if err != nil {
		fields := map[string]interface{}{"volumeID": volumeID}
		return goof.WithFieldsE(fields, "error removing volume", err)
	}
	return nil
}

var (
	errMissingNextDevice  = goof.New("missing next device")
	errVolAlreadyAttached = goof.New("volume already attached to a host")
)

// VolumeAttach attaches a volume and provides a token clients can use
// to validate that device has appeared locally.
func (d *driver) VolumeAttach(ctx types.Context, volumeID string, opts *types.VolumeAttachOpts) (*types.Volume, string, error) {
	vmName := context.MustInstanceID(ctx).ID
	fields := map[string]interface{}{
		"vmName":   vmName,
		"volumeID": volumeID,
	}

	volume, err := d.getVolume(ctx, volumeID,
		types.VolumeAttachmentsRequested)
	if err != nil {
		if _, ok := err.(*types.ErrNotFound); ok {
			return nil, "", err
		}
		return nil, "", goof.WithFieldsE(fields, "failed to get volume for attach", err)
	}
	// Check if volume is already attached
	// TODO: maybe add the check that new instance is the same as current
	if len(volume.Attachments) > 0 {
		// Detach already attached volume if forced
		if !opts.Force {
			return nil, "", errVolAlreadyAttached
		}
		for _, att := range volume.Attachments {
			err = d.detachDisk(ctx, volumeID, att.InstanceID.ID)
			if err != nil {
				return nil, "", goof.WithError("failed to detach volume first", err)
			}
		}
	}

	vm, err := d.getVM(ctx, vmName)
	if err != nil {
		return nil, "", goof.WithFieldsE(fields, "VM could not be obtained", err)
	}

	lun, err := d.attachDisk(ctx, volumeID, volume.Size, vm)
	if err != nil {
		return nil, "", goof.WithFieldsE(fields, "failed to attach volume", err)
	}

	volume, err = d.getVolume(ctx, volumeID,
		types.VolumeAttachmentsRequested)
	if err != nil {
		return nil, "", goof.WithFieldsE(fields, "failed to get just created/attached volume", err)
	}

	return volume, lun, nil
}

var errVolAlreadyDetached = goof.New("volume already detached")

// VolumeDetach detaches a volume.
func (d *driver) VolumeDetach(ctx types.Context, volumeID string, opts *types.VolumeDetachOpts) (*types.Volume, error) {
	vmName := context.MustInstanceID(ctx).ID
	fields := map[string]interface{}{
		"vmName":   vmName,
		"volumeID": volumeID,
	}

	volume, err := d.getVolume(ctx, volumeID,
		types.VolumeAttachmentsRequested)
	if err != nil {
		if _, ok := err.(*types.ErrNotFound); ok {
			return nil, err
		}
		return nil, goof.WithFieldsE(fields,
			"failed to get volume", err)
	}
	if len(volume.Attachments) == 0 {
		return nil, errVolAlreadyDetached
	}

	err = d.detachDisk(ctx, volumeID, vmName)
	if err != nil {
		return nil, err
	}

	volume, err = d.getVolume(ctx, volumeID,
		types.VolumeAttachmentsRequested)
	if err != nil {
		return nil, goof.WithFieldsE(fields,
			"failed to get volume", err)
	}
	return volume, nil
}

// Snapshots returns all volumes or a filtered list of snapshots.
func (d *driver) Snapshots(
	ctx types.Context,
	opts types.Store) ([]*types.Snapshot, error) {
	// TODO Snapshots are not implemented yet
	return nil, types.ErrNotImplemented
}

// SnapshotInspect inspects a single snapshot.
func (d *driver) SnapshotInspect(
	ctx types.Context,
	snapshotID string,
	opts types.Store) (*types.Snapshot, error) {
	// TODO Snapshots are not implemented yet
	return nil, types.ErrNotImplemented
}

// SnapshotCopy copies an existing snapshot.
func (d *driver) SnapshotCopy(
	ctx types.Context,
	snapshotID, snapshotName, destinationID string,
	opts types.Store) (*types.Snapshot, error) {
	// TODO Snapshots are not implemented yet
	return nil, types.ErrNotImplemented
}

// SnapshotRemove removes a snapshot.
func (d *driver) SnapshotRemove(
	ctx types.Context,
	snapshotID string,
	opts types.Store) error {
	// TODO Snapshots are not implemented yet
	return types.ErrNotImplemented
}

// Get volume or snapshot name without config tag
func (d *driver) getPrintableName(name string) string {
	return strings.TrimPrefix(name, d.tag()+azureud.TagDelimiter)
}

// Prefix volume or snapshot name with config tag
func (d *driver) getFullName(name string) string {
	if d.tag() != "" {
		return d.tag() + azureud.TagDelimiter + name
	}
	return name
}

// Retrieve config arguments
func (d *driver) getSubscriptionID() string {
	return d.config.GetString(azureud.ConfigAzureSubscriptionIDKey)
}

func (d *driver) getResourceGroup() string {
	return d.config.GetString(azureud.ConfigAzureResourceGroupKey)
}

func (d *driver) getTenantID() string {
	return d.config.GetString(azureud.ConfigAzureTenantIDKey)
}

func (d *driver) getStorageAccount() string {
	return d.config.GetString(azureud.ConfigAzureStorageAccountKey)
}

func (d *driver) getStorageAccessKey() string {
	return d.config.GetString(azureud.ConfigAzureStorageAccessKey)
}

func (d *driver) getContainer() string {
	return d.config.GetString(azureud.ConfigAzureContainerKey)
}

func (d *driver) getClientID() string {
	return d.config.GetString(azureud.ConfigAzureClientIDKey)
}

func (d *driver) getClientSecret() string {
	return d.config.GetString(azureud.ConfigAzureClientSecretKey)
}

func (d *driver) getCertPath() string {
	return d.config.GetString(azureud.ConfigAzureCertPathKey)
}

func (d *driver) getUseHTTPS() bool {
	return d.config.GetBool(azureud.ConfigAzureUseHTTPSKey)
}

func (d *driver) tag() string {
	return d.config.GetString(azureud.ConfigAzureTagKey)
}

// TODO rexrayTag
/*func (d *driver) rexrayTag() string {
  return d.config.GetString("azure.rexrayTag")
}*/

var errGetLocDevs = goof.New("error getting local devices from context")

func (d *driver) toTypesVolume(
	ctx types.Context,
	blobs []*container.BlobItem,
	attachments types.VolumeAttachmentsTypes) ([]*types.Volume, error) {

	var (
		ld      *types.LocalDevices
		ldOK    bool
		volumes []*types.Volume
		iid     *types.InstanceID
		vmDisks []*armcompute.DataDisk
	)

	if attachments.Devices() {
		if ld, ldOK = context.LocalDevices(ctx); !ldOK {
			return nil, errGetLocDevs
		}

		// We will need to query the VM to get its list of
		// attached disks, to match on the LUN number
		iid = context.MustInstanceID(ctx)
		vm, err := d.getVM(ctx, iid.ID)
		if err != nil {
			return nil, goof.WithError("Unable to lookup devices on VM", err)
		}
		vmDisks = vm.Properties.StorageProfile.DataDisks
	}

	// Metadata can have these fields:
	// microsoftazurecompute_resourcegroupname:trex
	// microsoftazurecompute_vmname:ttt
	// microsoftazurecompute_disktype:DataDisk (or OSDisk)
	// microsoftazurecompute_diskid:7d9df1c9-7b4f-41d4-a2e3-6870dfa573ba
	// microsoftazurecompute_diskname:ttt-20161221-130722
	// microsoftazurecompute_disksizeingb:50

	for _, blob := range blobs {

		btype := *blob.Metadata["microsoftazurecompute_disktype"]
		if btype == "" && !strings.HasSuffix(*blob.Name, vhdExtension) {
			continue
		}
		if btype == "OSDisk" {
			continue
		}

		bName := strings.TrimSuffix(*blob.Name, vhdExtension)

		volume := &types.Volume{
			Name: bName,
			ID:   *blob.Name,
			Type: btype,
			Size: *blob.Properties.ContentLength / size1GB,
			// TODO:
			//AvailabilityZone: *volume.AvailabilityZone,
			Encrypted: *blob.Properties.ServerEncrypted,
		}

		if attachments.Requested() {
			var attachedVols []*types.VolumeAttachment
			attVM := *blob.Metadata["microsoftazurecompute_vmname"]
			if attVM != "" {
				att := &types.VolumeAttachment{
					VolumeID: *blob.Name,
					InstanceID: &types.InstanceID{
						ID:     attVM,
						Driver: azureud.Name,
					},
				}
				if attachments.Devices() {
					if iid.ID == attVM {
						att.DeviceName = getDevice(ctx, vmDisks, &bName, ld.DeviceMap)
					}
				}
				attachedVols = append(attachedVols, att)
				volume.Attachments = attachedVols
			}
		}

		volumes = append(volumes, volume)
	}
	return volumes, nil
}

func getLunStr(lun int32) string {
	return strconv.FormatInt(int64(lun), 10)
}

func getDevice(
	ctx types.Context,
	vmDisks []*armcompute.DataDisk,
	bName *string,
	devMap map[string]string) string {

	for _, disk := range vmDisks {
		name := strings.TrimSuffix(*disk.Name, vhdExtension)
		if name == *bName {
			strLun := getLunStr(*disk.Lun)
			ctx.Debugf("Found matching disk %v on LUN %v on "+
				"instance, looking up dev from %v",
				name, strLun, devMap)
			for lun, dev := range devMap {
				if lun == strLun {
					return dev
				}
			}
		}
	}
	return ""
}

func (d *driver) diskURI(name string) string {
	scheme := "http"
	if d.useHTTPS {
		scheme = "https"
	}
	host := fmt.Sprintf("%s://%s.%s.%s", scheme, d.storageAccount,
		blobServiceName, autorest.PublicCloud.StorageEndpointSuffix)
	uri := fmt.Sprintf("%s/%s/%s", host, d.container, name)
	return uri
}

func (d *driver) getVM(ctx types.Context, name string) (*armcompute.VirtualMachine, error) {
	fields := map[string]interface{}{
		"vmName": name,
	}
	vmClient := mustSession(ctx).computeClientFactory.NewVirtualMachinesClient()
	resp, err := vmClient.Get(ctx, d.resourceGroup, name, nil)
	if err != nil {
		return nil, goof.WithFieldsE(fields, "failed to get virtual machine", err)
	}
	return &resp.VirtualMachine, nil
}

func (d *driver) getVolume(
	ctx types.Context,
	volumeID string,
	attachments types.VolumeAttachmentsTypes) (*types.Volume, error) {

	pager := mustSession(ctx).blobStorageClient.NewListBlobsFlatPager(d.container,
		&container.ListBlobsFlatOptions{
			Prefix:  &volumeID,
			Include: container.ListBlobsInclude{Metadata: true},
		})

	var blobs []*container.BlobItem
	for pager.More() {
		resp, err := pager.NextPage(ctx)
		if err != nil {
			return nil, goof.WithError("error listing blobs", err)
		}
		blobs = append(blobs, resp.Segment.BlobItems...)
	}

	if len(blobs) == 0 {
		return nil, apiUtils.NewNotFoundError(volumeID)
	}
	if len(blobs) > 1 {
		return nil, goof.New("multiple volumes found")
	}

	// Convert retrieved volumes to libStorage types.Volume
	vols, err := d.toTypesVolume(ctx, blobs, attachments)
	if err != nil {
		return nil, goof.WithError("failed to convert volume", err)
	}
	return vols[0], nil
}

func (d *driver) createDiskBlob(ctx types.Context, name string, size int64) error {
	pageBlobClient := mustSession(ctx).blobStorageClient.ServiceClient().NewContainerClient(d.container).NewPageBlobClient(name)

	// create VHD signature
	vhdHeader := vhd.CreateFixedHeader(uint64(size), &vhd.VHDOptions{})
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.BigEndian, vhdHeader); err != nil {
		return goof.WithError("Vhd header could not be created.", err)
	}
	header := buf.Bytes()

	// create new blob
	vhdSize := size + vhd.VHD_HEADER_SIZE
	if _, err := pageBlobClient.Create(ctx, vhdSize, nil); err != nil {
		return goof.WithError("PageBlob could not be created.", err)
	}

	// add VHD signature
	_, err := pageBlobClient.UploadPages(ctx,
		streaming.NopCloser(bytes.NewReader(header[:vhd.VHD_HEADER_SIZE])),
		blob.HTTPRange{Offset: size, Count: vhd.VHD_HEADER_SIZE}, nil)
	if err != nil {
		mustSession(ctx).blobStorageClient.DeleteBlob(ctx, d.container, name, nil)
		return goof.WithError("Vhd header could not be updated in the blob.", err)
	}

	return nil
}

func (d *driver) getNextDiskLun(vm *armcompute.VirtualMachine) (int32, error) {
	// 64 is a max number of LUNs per VM
	used := make([]bool, 64)
	disks := vm.Properties.StorageProfile.DataDisks
	for _, disk := range disks {
		if disk.Lun != nil {
			used[*disk.Lun] = true
		}
	}
	for k, v := range used {
		if !v {
			return int32(k), nil
		}
	}
	return -1, goof.New("Free Lun could not be found.")
}

func (d *driver) attachDisk(ctx types.Context, volumeName string, size int64, vm *armcompute.VirtualMachine) (string, error) {
	lun, err := d.getNextDiskLun(vm)
	if err != nil {
		return "", goof.WithError("Could not find find an empty Lun to attach disk to.", err)
	}

	uri := d.diskURI(volumeName)
	disks := vm.Properties.StorageProfile.DataDisks
	sizeGB := int32(size)
	disks = append(disks, &armcompute.DataDisk{
		Name:         &volumeName,
		Vhd:          &armcompute.VirtualHardDisk{URI: &uri},
		Lun:          &lun,
		CreateOption: to.Ptr(armcompute.DiskCreateOptionTypesAttach),
		DiskSizeGB:   &sizeGB,
		// TODO:
		// Caching:      cachingMode,
	})
	newVM := armcompute.VirtualMachine{
		Location: vm.Location,
		Properties: &armcompute.VirtualMachineProperties{
			StorageProfile: &armcompute.StorageProfile{
				DataDisks: disks,
			},
		},
	}

	poller, err := mustSession(ctx).computeClientFactory.NewVirtualMachinesClient().
		BeginCreateOrUpdate(ctx, d.resourceGroup, *vm.Name, newVM, nil)
	if err != nil {
		return "", goof.WithError("failed to request volume attachment to VM", err)
	}
	_, err = poller.PollUntilDone(ctx, nil)
	if err != nil {
		detail := err.Error()
		if strings.Contains(detail, "Code=\"AcquireDiskLeaseFailed\"") {
			// if lease cannot be acquired, immediately detach
			// the disk and return the original error
			ctx.Info("failed to acquire disk lease, try detach")
			_, _ = d.VolumeDetach(ctx, volumeName, nil)
		}
		return "", goof.WithError("failed to attach volume to VM", err)
	}

	return getLunStr(lun), nil
}

func (d *driver) detachDisk(ctx types.Context, volumeID string, vmName string) error {
	vm, err := d.getVM(ctx, vmName)
	if err != nil {
		return goof.WithError("VM could not be obtained", err)
	}

	found := false
	disks := vm.Properties.StorageProfile.DataDisks
	for i, disk := range disks {
		// Disk is paged blob in Azure. So blob name is case sensitive.
		if disk.Name != nil && *disk.Name == volumeID {
			ctx.Debugf("Removing %v from VM", volumeID)
			// found the disk
			disks = append(disks[:i], disks[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		return goof.New("VolumeID not found on given instance")
	}
	newVM := armcompute.VirtualMachine{
		Location: vm.Location,
		Properties: &armcompute.VirtualMachineProperties{
			StorageProfile: &armcompute.StorageProfile{
				DataDisks: disks,
			},
		},
	}

	poller, err := mustSession(ctx).computeClientFactory.NewVirtualMachinesClient().
		BeginCreateOrUpdate(ctx, d.resourceGroup, *vm.Name, newVM, nil)
	if err != nil {
		return goof.WithError("failed to request volume detachment from VM", err)
	}
	_, err = poller.PollUntilDone(ctx, nil)
	if err != nil {
		return goof.WithError("failed to detach volume from VM", err)
	}

	return nil
}
