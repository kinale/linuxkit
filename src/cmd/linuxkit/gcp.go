package main

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/moby/term"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/context"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/api/storage/v1"
)

const (
	pollingInterval = 500 * time.Millisecond
	timeout         = 300

	uefiCompatibleFeature = "UEFI_COMPATIBLE"
	vmxImageLicence       = "projects/vm-options/global/licenses/enable-vmx"
)

// GCPClient contains state required for communication with GCP
type GCPClient struct {
	client      *http.Client
	compute     *compute.Service
	storage     *storage.Service
	projectName string
	privKey     *rsa.PrivateKey
}

// NewGCPClient creates a new GCP client
func NewGCPClient(keys, projectName string) (*GCPClient, error) {
	log.Debugf("Connecting to GCP")
	ctx := context.Background()
	var client *GCPClient
	if projectName == "" {
		return nil, fmt.Errorf("the project name is not specified")
	}
	if keys != "" {
		log.Debugf("Using Keys %s", keys)
		f, err := os.Open(keys)
		if err != nil {
			return nil, err
		}

		jsonKey, err := io.ReadAll(f)
		if err != nil {
			return nil, err
		}

		config, err := google.JWTConfigFromJSON(jsonKey,
			storage.DevstorageReadWriteScope,
			compute.ComputeScope,
		)
		if err != nil {
			return nil, err
		}

		client = &GCPClient{
			client:      config.Client(ctx),
			projectName: projectName,
		}
	} else {
		log.Debugf("Using Application Default credentials")
		gc, err := google.DefaultClient(
			ctx,
			storage.DevstorageReadWriteScope,
			compute.ComputeScope,
		)
		if err != nil {
			return nil, err
		}
		client = &GCPClient{
			client:      gc,
			projectName: projectName,
		}
	}

	var err error
	client.compute, err = compute.NewService(ctx, option.WithHTTPClient(client.client))
	if err != nil {
		return nil, err
	}

	client.storage, err = storage.NewService(ctx, option.WithHTTPClient(client.client))
	if err != nil {
		return nil, err
	}

	log.Debugf("Generating SSH Keypair")
	client.privKey, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	return client, nil
}

// UploadFile uploads a file to Google Storage
func (g GCPClient) UploadFile(src, dst, bucketName string, public bool) error {
	log.Infof("Uploading file %s to Google Storage as %s", src, dst)
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	objectCall := g.storage.Objects.Insert(bucketName, &storage.Object{Name: dst}).Media(f)

	if public {
		objectCall.PredefinedAcl("publicRead")
	}

	_, err = objectCall.Do()
	if err != nil {
		return err
	}
	log.Infof("Upload Complete!")
	fmt.Println("gs://" + bucketName + "/" + dst)
	return nil
}

// CreateImage creates a GCP image using the source from Google Storage
func (g GCPClient) CreateImage(name, storageURL, family string, nested, uefi, replace bool) error {
	if replace {
		if err := g.DeleteImage(name); err != nil {
			return err
		}
	}

	log.Infof("Creating image: %s", name)
	imgObj := &compute.Image{
		RawDisk: &compute.ImageRawDisk{
			Source: storageURL,
		},
		Name: name,
	}

	if family != "" {
		imgObj.Family = family
	}

	if nested {
		imgObj.Licenses = []string{vmxImageLicence}
	}

	if uefi {
		imgObj.GuestOsFeatures = []*compute.GuestOsFeature{
			{Type: uefiCompatibleFeature},
		}
	}

	op, err := g.compute.Images.Insert(g.projectName, imgObj).Do()
	if err != nil {
		return err
	}

	if err := g.pollOperationStatus(op.Name); err != nil {
		return err
	}
	log.Infof("Image %s created", name)
	return nil
}

// DeleteImage deletes and image
func (g GCPClient) DeleteImage(name string) error {
	var notFound bool
	op, err := g.compute.Images.Delete(g.projectName, name).Do()
	if err != nil {
		if _, ok := err.(*googleapi.Error); !ok {
			return err
		}
		if err.(*googleapi.Error).Code != 404 {
			return err
		}
		notFound = true
	}
	if !notFound {
		log.Infof("Deleting existing image...")
		if err := g.pollOperationStatus(op.Name); err != nil {
			return err
		}
		log.Infof("Image %s deleted", name)
	}
	return nil
}

// CreateInstance creates and starts an instance on GCP
func (g GCPClient) CreateInstance(name, image, zone, machineType string, disks Disks, data *string, nested, vtpm, replace bool) error {
	if replace {
		if err := g.DeleteInstance(name, zone, true); err != nil {
			return err
		}
	}

	log.Infof("Creating instance %s from image %s (type: %s in %s)", name, image, machineType, zone)

	enabled := new(string)
	*enabled = "1"

	k, err := ssh.NewPublicKey(g.privKey.Public())
	if err != nil {
		return err
	}
	sshKey := new(string)
	*sshKey = fmt.Sprintf("moby:%s moby", string(ssh.MarshalAuthorizedKey(k)))

	// check provided image to be compatible with provided options
	op, err := g.compute.Images.Get(g.projectName, image).Do()
	if err != nil {
		return err
	}
	uefiCompatible := false
	for _, feature := range op.GuestOsFeatures {
		if feature != nil && feature.Type == uefiCompatibleFeature {
			uefiCompatible = true
			break
		}
	}
	if vtpm && !uefiCompatible {
		return fmt.Errorf("cannot use vTPM without UEFI_COMPATIBLE image")
	}
	// we should check for nested
	vmxLicense := false
	for _, license := range op.Licenses {
		// we omit hostname and version when define license
		if strings.HasSuffix(license, vmxImageLicence) {
			vmxLicense = true
			break
		}
	}
	if nested && !vmxLicense {
		return fmt.Errorf("cannot use nested virtualization without enable-vmx image")
	}

	instanceDisks := []*compute.AttachedDisk{
		{
			AutoDelete: true,
			Boot:       true,
			InitializeParams: &compute.AttachedDiskInitializeParams{
				SourceImage: fmt.Sprintf("global/images/%s", image),
			},
		},
	}

	for i, disk := range disks {
		var diskName string
		if disk.Path != "" {
			diskName = disk.Path
		} else {
			diskName = fmt.Sprintf("%s-disk-%d", name, i)
		}
		var diskSizeGb int64
		if disk.Size == 0 {
			diskSizeGb = int64(1)
		} else {
			diskSizeGb = int64(convertMBtoGB(disk.Size))
		}
		diskObj := &compute.Disk{Name: diskName, SizeGb: diskSizeGb}
		if vtpm {
			diskObj.GuestOsFeatures = []*compute.GuestOsFeature{
				{Type: uefiCompatibleFeature},
			}
		}
		diskOp, err := g.compute.Disks.Insert(g.projectName, zone, diskObj).Do()
		if err != nil {
			return err
		}
		if err := g.pollZoneOperationStatus(diskOp.Name, zone); err != nil {
			return err
		}
		instanceDisks = append(instanceDisks, &compute.AttachedDisk{
			AutoDelete: true,
			Boot:       false,
			Source:     fmt.Sprintf("zones/%s/disks/%s", zone, diskName),
		})
	}

	instanceObj := &compute.Instance{
		MachineType: fmt.Sprintf("zones/%s/machineTypes/%s", zone, machineType),
		Name:        name,
		Disks:       instanceDisks,
		NetworkInterfaces: []*compute.NetworkInterface{
			{
				Network: "global/networks/default",
				AccessConfigs: []*compute.AccessConfig{
					{
						Type: "ONE_TO_ONE_NAT",
					},
				},
			},
		},
		Metadata: &compute.Metadata{
			Items: []*compute.MetadataItems{
				{
					Key:   "serial-port-enable",
					Value: enabled,
				},
				{
					Key:   "ssh-keys",
					Value: sshKey,
				},
				{
					Key:   "user-data",
					Value: data,
				},
			},
		},
	}

	if nested {
		instanceObj.MinCpuPlatform = "Intel Haswell"
	}
	if vtpm {
		instanceObj.ShieldedInstanceConfig = &compute.ShieldedInstanceConfig{EnableVtpm: true}
	}

	// Don't wait for operation to complete!
	// A headstart is needed as by the time we've polled for this event to be
	// completed, the instance may have already terminated
	_, err = g.compute.Instances.Insert(g.projectName, zone, instanceObj).Do()
	if err != nil {
		return err
	}
	log.Infof("Instance created")
	return nil
}

// DeleteInstance removes an instance
func (g GCPClient) DeleteInstance(instance, zone string, wait bool) error {
	var notFound bool
	op, err := g.compute.Instances.Delete(g.projectName, zone, instance).Do()
	if err != nil {
		if _, ok := err.(*googleapi.Error); !ok {
			return err
		}
		if err.(*googleapi.Error).Code != 404 {
			return err
		}
		notFound = true
	}
	if !notFound && wait {
		log.Infof("Deleting existing instance...")
		if err := g.pollZoneOperationStatus(op.Name, zone); err != nil {
			return err
		}
		log.Infof("Instance %s deleted", instance)
	}
	return nil
}

// GetInstanceSerialOutput streams the serial output of an instance
func (g GCPClient) GetInstanceSerialOutput(instance, zone string) error {
	log.Infof("Getting serial port output for instance %s", instance)
	var next int64
	for {
		res, err := g.compute.Instances.GetSerialPortOutput(g.projectName, zone, instance).Start(next).Do()
		if err != nil {
			if err.(*googleapi.Error).Code == 400 {
				// Instance may not be ready yet...
				time.Sleep(pollingInterval)
				continue
			}
			if err.(*googleapi.Error).Code == 503 {
				// Timeout received when the instance has terminated
				break
			}
			return err
		}
		fmt.Printf("%s\n", res.Contents)
		next = res.Next
		// When the instance has been stopped, Start and Next will both be 0
		if res.Start > 0 && next == 0 {
			break
		}
	}
	return nil
}

// ConnectToInstanceSerialPort uses SSH to connect to the serial port of the instance
func (g GCPClient) ConnectToInstanceSerialPort(instance, zone string) error {
	log.Infof("Connecting to serial port of instance %s", instance)
	gPubKeyURL := "https://cloud-certs.storage.googleapis.com/google-cloud-serialport-host-key.pub"
	resp, err := http.Get(gPubKeyURL)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	gPubKey, _, _, _, err := ssh.ParseAuthorizedKey(body)
	if err != nil {
		return err
	}

	signer, err := ssh.NewSignerFromKey(g.privKey)
	if err != nil {
		return err
	}
	config := &ssh.ClientConfig{
		User: fmt.Sprintf("%s.%s.%s.moby", g.projectName, zone, instance),
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.FixedHostKey(gPubKey),
		Timeout:         5 * time.Second,
	}

	var conn *ssh.Client
	// Retry connection as VM may not be ready yet
	for i := 0; i < timeout; i++ {
		conn, err = ssh.Dial("tcp", "ssh-serialport.googleapis.com:9600", config)
		if err != nil {
			time.Sleep(pollingInterval)
			continue
		}
		break
	}
	if conn == nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	session, err := conn.NewSession()
	if err != nil {
		return err
	}
	defer func() { _ = session.Close() }()

	stdin, err := session.StdinPipe()
	if err != nil {
		return fmt.Errorf("unable to setup stdin for session: %v", err)
	}
	go func() {
		_, _ = io.Copy(stdin, os.Stdin)
	}()

	stdout, err := session.StdoutPipe()
	if err != nil {
		return fmt.Errorf("unable to setup stdout for session: %v", err)
	}
	go func() {
		_, _ = io.Copy(os.Stdout, stdout)
	}()

	stderr, err := session.StderrPipe()
	if err != nil {
		return fmt.Errorf("unable to setup stderr for session: %v", err)
	}
	go func() {
		_, _ = io.Copy(os.Stderr, stderr)
	}()
	/*
		c := make(chan os.Signal, 1)
		exit := make(chan bool, 1)
		signal.Notify(c)
		go func(exit <-chan bool, c <-chan os.Signal) {
			select {
			case <-exit:
				return
			case s := <-c:
				switch s {
				// CTRL+C
				case os.Interrupt:
					session.Signal(ssh.SIGINT)
				// CTRL+\
				case os.Kill:
					session.Signal(ssh.SIGQUIT)
				default:
					log.Debugf("Received signal %s but not forwarding to ssh", s)
				}
			}
		}(exit, c)
	*/
	var termWidth, termHeight int
	fd := os.Stdin.Fd()

	if term.IsTerminal(fd) {
		oldState, err := term.MakeRaw(fd)
		if err != nil {
			return err
		}

		defer func() {
			_ = term.RestoreTerminal(fd, oldState)
		}()

		winsize, err := term.GetWinsize(fd)
		if err != nil {
			termWidth = 80
			termHeight = 24
		} else {
			termWidth = int(winsize.Width)
			termHeight = int(winsize.Height)
		}
	}

	if err = session.RequestPty("xterm", termHeight, termWidth, ssh.TerminalModes{
		ssh.ECHO: 1,
	}); err != nil {
		return err
	}

	if err = session.Shell(); err != nil {
		return err
	}

	err = session.Wait()
	//exit <- true
	if err != nil {
		return err
	}
	return nil
}

func (g *GCPClient) pollOperationStatus(operationName string) error {
	for i := 0; i < timeout; i++ {
		operation, err := g.compute.GlobalOperations.Get(g.projectName, operationName).Do()
		if err != nil {
			return fmt.Errorf("error fetching operation status: %v", err)
		}
		if operation.Error != nil {
			return fmt.Errorf("error running operation: %v", operation.Error)
		}
		if operation.Status == "DONE" {
			return nil
		}
		time.Sleep(pollingInterval)
	}
	return fmt.Errorf("timeout waiting for operation to finish")

}
func (g *GCPClient) pollZoneOperationStatus(operationName, zone string) error {
	for i := 0; i < timeout; i++ {
		operation, err := g.compute.ZoneOperations.Get(g.projectName, zone, operationName).Do()
		if err != nil {
			return fmt.Errorf("error fetching operation status: %v", err)
		}
		if operation.Error != nil {
			return fmt.Errorf("error running operation: %v", operation.Error)
		}
		if operation.Status == "DONE" {
			return nil
		}
		time.Sleep(pollingInterval)
	}
	return fmt.Errorf("timeout waiting for operation to finish")
}
