package linode

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform/helper/schema"
	"github.com/taoh/linodego"
	"golang.org/x/crypto/sha3"
)

const (
	LINODE_BEING_CREATED = -1
	LINODE_BRAND_NEW     = 0
	LINODE_RUNNING       = 1
	LINODE_POWERED_OFF   = 2
)

var (
	kernelList        *[]linodego.Kernel
	regionList        *[]linodego.DataCenter
	sizeList          *[]linodego.LinodePlan
	latestKernelStrip *regexp.Regexp
)

func init() {
	latestKernelStrip = regexp.MustCompile("\\s*\\(.*\\)\\s*")
}

func resourceLinodeLinode() *schema.Resource {
	return &schema.Resource{
		Create: resourceLinodeLinodeCreate,
		Read:   resourceLinodeLinodeRead,
		Update: resourceLinodeLinodeUpdate,
		Delete: resourceLinodeLinodeDelete,
		Schema: map[string]*schema.Schema{
			"image": &schema.Schema{
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},
			"kernel": &schema.Schema{
				Type:     schema.TypeString,
				Required: true,
			},
			"name": &schema.Schema{
				Type:     schema.TypeString,
				Optional: true,
			},
			"group": &schema.Schema{
				Type:     schema.TypeString,
				Optional: true,
				Default:  "Linode",
			},
			"region": &schema.Schema{
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},
			"size": &schema.Schema{
				Type:     schema.TypeInt,
				Required: true,
			},
			"status": &schema.Schema{
				Type:     schema.TypeInt,
				Computed: true,
			},
			"plan_storage": &schema.Schema{
				Type:     schema.TypeInt,
				Computed: true,
			},
			"plan_storage_utilized": &schema.Schema{
				Type:     schema.TypeInt,
				Computed: true,
			},
			"ip_address": &schema.Schema{
				Type:     schema.TypeString,
				Computed: true,
			},
			"private_networking": &schema.Schema{
				Type:     schema.TypeBool,
				Optional: true,
			},
			"private_ip_address": &schema.Schema{
				Type:     schema.TypeString,
				Computed: true,
			},
			"ssh_key": &schema.Schema{
				Type:      schema.TypeString,
				Required:  true,
				ForceNew:  true,
				StateFunc: sshKeyState,
			},
			"root_password": &schema.Schema{
				Type:      schema.TypeString,
				Required:  true,
				ForceNew:  true,
				StateFunc: rootPasswordState,
			},
			"helper_distro": &schema.Schema{
				Type:     schema.TypeBool,
				Optional: true,
				Default:  true,
			},
			"manage_private_ip_automatically": &schema.Schema{
				Type:     schema.TypeBool,
				Optional: true,
				Default:  true,
			},
			"disk_expansion": &schema.Schema{
				Type:     schema.TypeBool,
				Optional: true,
				Default:  false,
			},
			"swap_size": &schema.Schema{
				Type:     schema.TypeInt,
				Optional: true,
				Default:  512,
			},
		},
	}
}

func resourceLinodeLinodeRead(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*linodego.Client)
	id, err := strconv.ParseInt(d.Id(), 10, 64)
	if err != nil {
		return fmt.Errorf("Failed to parse %s as int because %s", d.Id(), err)
	}
	linodes, err := client.Linode.List(int(id))
	if err != nil {
		return fmt.Errorf("Failed to find the specified linode because %s", err)
	}
	if len(linodes.Errors) != 0 {
		var output = ""
		for _, value := range linodes.Errors {
			output = fmt.Sprintf("%s\n%s", output, value)
		}
		return fmt.Errorf("Failed to find the specified linode. The following errors occured: %s", output)
	}
	if len(linodes.Linodes) != 1 {
		d.SetId("")
		return nil
	}
	linode := linodes.Linodes[0]
	public, private, err := getIps(client, int(id))
	if err != nil {
		return fmt.Errorf("Failed to get the ips for linode %s because %s", d.Id(), err)
	}
	d.Set("ip_address", public)
	if private != "" {
		d.Set("private_networking", true)
		d.Set("private_ip_address", private)
	} else {
		d.Set("private_networking", false)
	}

	d.SetConnInfo(map[string]string{
		"type": "ssh",
		"host": public,
	})

	d.Set("name", linode.Label)
	d.Set("group", linode.LpmDisplayGroup)

	regionName, err := getRegionName(client, linode.DataCenterId)
	if err != nil {
		return err
	}
	d.Set("region", regionName)

	size, err := getSize(client, linode.PlanId)
	if err != nil {
		return fmt.Errorf("Failed to find the size for linode %s because %s", d.Id(), err)
	}
	d.Set("size", size)

	d.Set("status", linode.Status)

	sizeId, err := getSizeId(client, d.Get("size").(int))
	if err != nil {
		return err
	}

	plan_storage, err := getPlanDiskSize(client, sizeId)
	if err != nil {
		return err
	}
	d.Set("plan_storage", plan_storage)

	plan_storage_utilized, err := getTotalDiskSize(client, linode.LinodeId)
	if err != nil {
		return err
	}
	d.Set("plan_storage_utilized", plan_storage_utilized)

	d.Set("disk_expansion", boolToString(d.Get("disk_expansion").(bool)))

	diskResp, err := client.Disk.List(linode.LinodeId, -1)
	if err != nil {
		return fmt.Errorf("Failed to get the disks for the linode because %s", err)
	}

	// Determine if swap exists and the size.  If it does not exist, swap_size=0
	swap_size := 0
	for i := range diskResp.Disks {
		if strings.EqualFold(diskResp.Disks[i].Type, "swap") {
			swap_size = diskResp.Disks[i].Size
		}
	}
	d.Set("swap_size", swap_size)

	configs, err := client.Config.List(int(id), -1)
	if err != nil {
		log.Printf("Configs: %v", configs)
		return fmt.Errorf("Failed to get the linode %s's (id %s) config because %s", d.Get("name").(string), d.Id(), err)
	} else if len(configs.LinodeConfigs) != 1 {
		return nil
	}
	config := configs.LinodeConfigs[0]

	d.Set("helper_distro", boolToString(config.HelperDistro.Bool))
	d.Set("manage_private_ip_automatically", boolToString(config.HelperDistro.Bool))

	kernelName, err := getKernelName(client, config.KernelId)
	if err != nil {
		return fmt.Errorf("Failed to find the kernel for linode %s because %s", d.Id(), err)
	}
	d.Set("kernel", kernelName)

	return nil
}

func resourceLinodeLinodeCreate(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*linodego.Client)
	d.Partial(true)

	regionId, err := getRegionID(client, d.Get("region").(string))
	if err != nil {
		return fmt.Errorf("Failed to locate region %s because %s", d.Get("region").(string), err)
	}

	sizeId, err := getSizeId(client, d.Get("size").(int))
	if err != nil {
		return fmt.Errorf("Failed to find a size %s because %s", d.Get("size"), err)
	}
	create, err := client.Linode.Create(regionId, sizeId, 1)
	if err != nil {
		return fmt.Errorf("Failed to create a linode in region %s of size %d because %s", d.Get("region"), d.Get("size"), err)
	}

	d.SetId(fmt.Sprintf("%d", create.LinodeId.LinodeId))
	d.SetPartial("region")
	d.SetPartial("size")

	// Create the Swap Partition
	if d.Get("swap_size").(int) > 0 {
		emptyArgs := make(map[string]string)
		_, err = client.Disk.Create(create.LinodeId.LinodeId, "swap", "swap", d.Get("swap_size").(int), emptyArgs)
		if err != nil {
			return fmt.Errorf("Failed to create a swap drive because %s", err)
		}
	}
	d.SetPartial("swap_size")

	// Load the basic data about the current linode
	linodes, err := client.Linode.List(create.LinodeId.LinodeId)
	if err != nil {
		return fmt.Errorf("Failed to load data about the newly created linode because %s", err)
	} else if len(linodes.Linodes) != 1 {
		return fmt.Errorf("An incorrect number of linodes (%d) was returned for id %s", len(linodes.Linodes), d.Id())
	}
	linode := linodes.Linodes[0]

	if err = changeLinodeSettings(client, linode, d); err != nil {
		return err
	}

	if d.Get("private_networking").(bool) {
		resp, err := client.Ip.AddPrivate(linode.LinodeId)
		if err != nil {
			return fmt.Errorf("Failed to add a private ip address to linode %d because %s", linode.LinodeId, err)
		}
		d.Set("private_ip_address", resp.IPAddress.IPAddress)
		d.SetPartial("private_ip_address")
	}
	d.SetPartial("private_networking")

	ssh_key := d.Get("ssh_key").(string)
	password := d.Get("root_password").(string)
	disk_size := (linode.TotalHD - d.Get("swap_size").(int))
	err = deployImage(client, linode, d.Get("image").(string), disk_size, ssh_key, password)
	if err != nil {
		return fmt.Errorf("Failed to create disk for image %s because %s", d.Get("image"), err)
	}

	d.SetPartial("root_password")
	d.SetPartial("ssh_key")

	diskResp, err := client.Disk.List(linode.LinodeId, -1)
	if err != nil {
		return fmt.Errorf("Failed to get the disks for the newly created linode because %s", err)
	}
	var rootDisk int
	var swapDisk int
	for i := range diskResp.Disks {
		if strings.EqualFold(diskResp.Disks[i].Type, "swap") {
			swapDisk = diskResp.Disks[i].DiskId
		} else {
			rootDisk = diskResp.Disks[i].DiskId
		}
	}

	kernelId, err := getKernelID(client, d.Get("kernel").(string))
	if err != nil {
		return fmt.Errorf("Failed to find kernel %s because %s", d.Get("kernel").(string), err)
	}

	confArgs := make(map[string]string)
	if d.Get("manage_private_ip_automatically").(bool) {
		confArgs["helper_network"] = "true"
	} else {
		confArgs["helper_network"] = "false"
	}
	if d.Get("helper_distro").(bool) {
		confArgs["helper_distro"] = "true"
	} else {
		confArgs["helper_distro"] = "false"
	}
	if d.Get("swap_size").(int) > 0 {
		confArgs["DiskList"] = fmt.Sprintf("%d,%d", rootDisk, swapDisk)
	} else {
		confArgs["DiskList"] = fmt.Sprintf("%d", rootDisk)
	}

	confArgs["RootDeviceNum"] = "1"
	c, err := client.Config.Create(linode.LinodeId, kernelId, d.Get("image").(string), confArgs)
	if err != nil {
		log.Printf("diskList: %s", confArgs["DiskList"])
		log.Println(confArgs["DiskList"])
		return fmt.Errorf("Failed to create config for linode %d because %s", linode.LinodeId, err)
	}
	confID := c.LinodeConfigId
	d.SetPartial("image")
	d.SetPartial("manage_private_ip_automatically")
	d.SetPartial("helper_distro")
	client.Linode.Boot(linode.LinodeId, confID.LinodeConfigId)

	d.Partial(false)
	err = waitForJobsToComplete(client, linode.LinodeId)
	if err != nil {
		return fmt.Errorf("Failed to wait for linode %d to boot because %s", linode.LinodeId, err)
	}

	return resourceLinodeLinodeRead(d, meta)
}

func resourceLinodeLinodeUpdate(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*linodego.Client)
	d.Partial(true)

	id, err := strconv.ParseInt(d.Id(), 10, 64)
	if err != nil {
		return fmt.Errorf("Failed to parse linode id %s as an int because %s", d.Id(), err)
	}

	linodes, err := client.Linode.List(int(id))
	if err != nil {
		return fmt.Errorf("Failed to fetch data about the current linode because %s", err)
	}
	linode := linodes.Linodes[0]

	if d.HasChange("name") || d.HasChange("group") {
		if err = changeLinodeSettings(client, linode, d); err != nil {
			return err
		}
	}

	if d.HasChange("size") {
		if err = changeLinodeSize(client, linode, d); err != nil {
			return err
		}
		if err = waitForJobsToComplete(client, int(id)); err != nil {
			return fmt.Errorf("Failed while waiting for linode %s to finish resizing because %s", d.Id(), err)
		}
	}

	configResp, err := client.Config.List(int(id), -1)
	if err != nil {
		return fmt.Errorf("Failed to fetch the config for linode %d because %s", id, err)
	}
	if len(configResp.LinodeConfigs) != 1 {
		return fmt.Errorf("Linode %d has an incorrect number of configs %d, this plugin can only handle 1", id, len(configResp.LinodeConfigs))
	}
	config := configResp.LinodeConfigs[0]

	if err = changeLinodeConfig(client, config, d); err != nil {
		return fmt.Errorf("Failed to update Linode %d config because %s", id, err)
	}

	if d.HasChange("private_networking") {
		if !d.Get("private_networking").(bool) {
			return fmt.Errorf("Can't deactivate private networking for linode %s", d.Id())
		} else {
			_, err = client.Ip.AddPrivate(int(id))
			if err != nil {
				return fmt.Errorf("Failed to activate private networking on linode %s because %s", d.Id(), err)
			}
			if d.Get("manage_private_ip_automatically").(bool) {
				_, err = client.Linode.Reboot(int(id), 0)
				if err != nil {
					return fmt.Errorf("Failed to reboot linode %s because %s", d.Id(), err)
				}
				err = waitForJobsToComplete(client, int(id))
				if err != nil {
					return fmt.Errorf("Failed while waiting for linode %s to finish rebooting because %s", d.Id(), err)
				}
			}
		}
	}

	return resourceLinodeLinodeRead(d, meta)
}

func resourceLinodeLinodeDelete(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*linodego.Client)
	id, err := strconv.ParseInt(d.Id(), 10, 64)
	if err != nil {
		return fmt.Errorf("Failed to parse linode id %d as int", d.Id())
	}
	_, err = client.Linode.Delete(int(id), true)
	if err != nil {
		return fmt.Errorf("Failed to delete linode %d because %s", id, err)
	}
	return nil
}

// getDisks gets all of the disks that are attached to the linode. It only returns the names of those disks
func getDisks(client *linodego.Client, id int) ([]string, error) {
	resp, err := client.Disk.List(id, -1)
	if err != nil {
		return []string{}, err
	}
	if len(resp.Disks) != 2 {
		return []string{}, fmt.Errorf("Found %d disks attached to linode %s. This plugin can only handle exactly 2.", len(resp.Disks), err)
	}
	disks := []string{}
	for i := range resp.Disks {
		disks = append(disks, resp.Disks[i].Label.String())
	}
	return disks, nil
}

// getImage Finds out what image was used to create the server.
func getImage(client *linodego.Client, id int) (string, error) {
	disks, err := getDisks(client, id)
	if err != nil {
		return "", err
	}

	// Assumes disk naming convention of Root(LINODEID)__Base(IMAGEID)
	grabId := regexp.MustCompile(`Base\(([0-9]+)\)`)

	for i := range disks {
		// Check if we match the pattern at all
		if grabId.MatchString(disks[i]) {
			// Print out the first group match
			return grabId.FindStringSubmatch(disks[i])[1], nil
		} else if strings.HasSuffix(disks[i], " Disk") {
			// Keep the old method for backward compatibility
			return disks[i][:(len(disks[i]) - 5)], nil
		}
	}
	return "", errors.New("Unable to find the image based on the disk names")
}

// getKernelName gets the name of the kernel from the id.
func getKernelName(client *linodego.Client, kernelId int) (string, error) {
	if kernelList == nil {
		if err := getKernelList(client); err != nil {
			return "", err
		}
	}
	k := *kernelList
	for i := range k {
		if k[i].KernelId == kernelId {
			if strings.HasPrefix(k[i].Label.String(), "Latest") {
				return latestKernelStrip.ReplaceAllString(k[i].Label.String(), ""), nil
			} else {
				return k[i].Label.String(), nil
			}
		}
	}
	return "", fmt.Errorf("Failed to find kernel id %d", kernelId)
}

// getKernelID gets the id of the kernel from the specified id.
func getKernelID(client *linodego.Client, kernelName string) (int, error) {
	if kernelList == nil {
		if err := getKernelList(client); err != nil {
			return -1, err
		}
	}
	k := *kernelList
	for i := range k {
		if strings.HasPrefix(kernelName, "Latest") {
			if strings.HasPrefix(k[i].Label.String(), kernelName) {
				return k[i].KernelId, nil
			}
		} else {
			if k[i].Label.String() == kernelName {
				return k[i].KernelId, nil
			}
		}
	}
	return -1, fmt.Errorf("Failed to find kernel %s", kernelName)
}

// getKernelList populates kernelList with all of the available kernels. kernelList is purely to reduce the number of
// api calls as the available kernels are unlikely to change within a single terraform run.
func getKernelList(client *linodego.Client) error {
	kernels, err := client.Avail.Kernels()
	if err != nil {
		return err
	}
	kernelList = &kernels.Kernels
	return nil
}

// getRegionName gets the region name from the region id
func getRegionName(client *linodego.Client, regionId int) (string, error) {
	if regionList == nil {
		if err := getRegionList(client); err != nil {
			return "", err
		}
	}

	r := *regionList
	for i := range r {
		if r[i].DataCenterId == regionId {
			return r[i].Location, nil
		}
	}
	return "", fmt.Errorf("Failed to find region id %d", regionId)
}

// getRegionID gets the region id from the name of the region
func getRegionID(client *linodego.Client, regionName string) (int, error) {
	if regionList == nil {
		if err := getRegionList(client); err != nil {
			return -1, err
		}
	}

	r := *regionList
	for i := range r {
		if r[i].Location == regionName {
			return r[i].DataCenterId, nil
		}
	}
	return -1, fmt.Errorf("Failed to find the region name %s", regionName)
}

// getRegionList populates regionList with the available regions. regionList is used to reduce the number of api
// requests required as it is unlikely that the available regions will change during a single terraform run.
func getRegionList(client *linodego.Client) error {
	resp, err := client.Avail.DataCenters()
	if err != nil {
		return err
	}
	regionList = &resp.DataCenters
	return nil
}

// getSizeId gets the id from the specified amount of ram
func getSizeId(client *linodego.Client, size int) (int, error) {
	if sizeList == nil {
		if err := getSizeList(client); err != nil {
			return -1, err
		}
	}

	s := *sizeList
	for i := range s {
		if s[i].RAM == size {
			return s[i].PlanId, nil
		}
	}
	return -1, fmt.Errorf("Unable to locate the plan with RAM %d", size)
}

// getSize gets the amount of ram from the plan id
func getSize(client *linodego.Client, sizeId int) (int, error) {
	if sizeList == nil {
		if err := getSizeList(client); err != nil {
			return -1, err
		}
	}

	s := *sizeList
	for i := range s {
		if s[i].PlanId == sizeId {
			return s[i].RAM, nil
		}
	}
	return -1, fmt.Errorf("Unabled to find plan id %d", sizeId)
}

// getSizeList populates sizeList. sizeList is used to reduce the number of api requests required as its unlikely that
// the plans will change during a single terraform run.
func getSizeList(client *linodego.Client) error {
	resp, err := client.Avail.LinodePlans()
	if err != nil {
		return err
	}
	sizeList = &resp.LinodePlans
	return nil
}

// getPlanDiskSizeList gets the total available disk space for the given PlanID
func getPlanDiskSize(client *linodego.Client, planID int) (int, error) {
	if sizeList == nil {
		if err := getSizeList(client); err != nil {
			return -1, err
		}
	}

	s := *sizeList
	for i := range s {
		if s[i].PlanId == planID {
			// Return Plan Disk Size in Megabytes
			return (s[i].Disk * 1024), nil
		}
	}
	return -1, fmt.Errorf("Unabled to find plan id %d", planID)
}

// getTotalDiskSize returns the number of disks and their total size.
func getTotalDiskSize(client *linodego.Client, linodeID int) (int, error) {
	var totalDiskSize int
	diskList, err := client.Disk.List(linodeID, -1)
	if err != nil {
		return -1, err
	}

	totalDiskSize = 0
	disks := diskList.Disks
	for i := range disks {
		// Calculate Total Disk Size
		totalDiskSize = totalDiskSize + disks[i].Size
	}

	return totalDiskSize, nil
}

// getBiggestDisk returns the ID and Size of the largest disk attached to the Linode
func getBiggestDisk(client *linodego.Client, linodeID int) (biggestDiskID int, biggestDiskSize int, err error) {
	// Retrieve the Linode's list of disks
	diskList, err := client.Disk.List(linodeID, -1)
	if err != nil {
		return -1, -1, err
	}

	biggestDiskID = 0
	biggestDiskSize = 0
	disks := diskList.Disks
	for i := range disks {
		// Find Biggest Disk ID & Size
		if disks[i].Size > biggestDiskSize {
			biggestDiskID = disks[i].DiskId
			biggestDiskSize = disks[i].Size
		}
	}
	return biggestDiskID, biggestDiskSize, nil
}

// getIps gets the ips assigned to the linode
func getIps(client *linodego.Client, linodeId int) (publicIp string, privateIp string, err error) {
	resp, err := client.Ip.List(linodeId, -1)
	if err != nil {
		return "", "", err
	}
	ips := resp.FullIPAddresses
	for i := range ips {
		if ips[i].IsPublic == 1 {
			publicIp = ips[i].IPAddress
		} else {
			privateIp = ips[i].IPAddress
		}
	}

	return publicIp, privateIp, nil
}

// sshKeyState hashes a string passed in as an interface
func sshKeyState(val interface{}) string {
	return hashString(val.(string))
}

// rootPasswordState hashes a string passed in as an interface
func rootPasswordState(val interface{}) string {
	return hashString(val.(string))
}

// hashString hashes a string
func hashString(key string) string {
	hash := sha3.Sum256([]byte(key))
	return base64.StdEncoding.EncodeToString(hash[:])
}

const (
	PREBUILT = iota
	CUSTOM_IMAGE
)

// findImage finds the specified image. It checks the prebuilt images first and then any custom images. It returns both
// the image type and the images id
func findImage(client *linodego.Client, imageName string) (imageType, imageId int, err error) {
	// Get Available Distributions
	distResp, err := client.Avail.Distributions()
	if err != nil {
		return -1, -1, err
	}
	prebuilt := distResp.Distributions
	for i := range prebuilt {
		if prebuilt[i].Label.String() == imageName {
			return PREBUILT, prebuilt[i].DistributionId, nil
		}
	}

	// Get Available Client Images
	custResp, err := client.Image.List()
	if err != nil {
		return -1, -1, err
	}
	customImages := custResp.Images
	for i := range customImages {
		if customImages[i].Label.String() == imageName {
			return CUSTOM_IMAGE, customImages[i].ImageId, nil
		}
		if strconv.Itoa(customImages[i].ImageId) == imageName {
			return CUSTOM_IMAGE, customImages[i].ImageId, nil
		}
	}

	return -1, -1, fmt.Errorf("Failed to find image %s", imageName)
}

// deployImage deploys the specified image
// DiskLabel has 50 characters maximum!!!
func deployImage(client *linodego.Client, linode linodego.Linode, imageName string, diskSize int, key, password string) error {
	imageType, imageId, err := findImage(client, imageName)
	if err != nil {
		return err
	}
	args := make(map[string]string)
	args["rootSSHKey"] = key
	args["rootPass"] = password
	diskLabel := fmt.Sprintf("Root(%d)__Base(%d)", linode.LinodeId, imageId)
	if imageType == PREBUILT {
		_, err = client.Disk.CreateFromDistribution(imageId, linode.LinodeId, diskLabel, diskSize, args)
		if err != nil {
			return err
		}
	} else if imageType == CUSTOM_IMAGE {
		_, err = client.Disk.CreateFromImage(imageId, linode.LinodeId, diskLabel, diskSize, args)
		if err != nil {
			return err
		}
	} else {
		panic("Invalid image type returned")
	}
	err = waitForJobsToComplete(client, linode.LinodeId)
	if err != nil {
		return fmt.Errorf("Image %d failed to thaw for linode %d because %s", imageId, linode.LinodeId, err)
	}
	return nil
}

// waitForJobsToComplete waits for all of the jobs on the specified linode to complete before returning. It will timeout
// after 5 minutes.
func waitForJobsToComplete(client *linodego.Client, linodeId int) error {
	start := time.Now()
	for {
		jobs, err := client.Job.List(linodeId, -1, false)
		if err != nil {
			return err
		}
		complete := true
		for i := range jobs.Jobs {
			if !jobs.Jobs[i].HostFinishDt.IsSet() {
				complete = false
			}
		}
		if complete {
			return nil
		}
		time.Sleep(1 * time.Second)
		if time.Since(start) > 5*time.Minute {
			return fmt.Errorf("Jobs for linode %d didn't complete in 5 minutes", linodeId)
		}
	}
}

// waitForJobsToCompleteWaitMinutes waits (waitMinutes) for all of the jobs on the specified linode to complete before returning.
// It will timeout after (waitMinutes) has elapsed.
func waitForJobsToCompleteWaitMinutes(client *linodego.Client, linodeId int, waitMinutes int) error {
	start := time.Now()
	for {
		jobs, err := client.Job.List(linodeId, -1, false)
		if err != nil {
			return err
		}
		complete := true
		for i := range jobs.Jobs {
			if !jobs.Jobs[i].HostFinishDt.IsSet() {
				complete = false
				// MAKEME: log.Printf("[INFO] JOB:(%s) %s", jobs.Jobs[i].Label, jobs.Jobs[i].HostMessage)
				// Can't figure out how to print this to the console. This would give the pending job status and ETA.
			}
		}
		if complete {
			return nil
		}
		time.Sleep(1 * time.Second)
		if time.Since(start) > (time.Duration(waitMinutes) * time.Minute) {
			return fmt.Errorf("Jobs for linode %d didn't complete in %d minute(s)", linodeId, waitMinutes)
		}
	}
}

// changeLinodeSettings changes linode level settings. This is things like the name or the group
func changeLinodeSettings(client *linodego.Client, linode linodego.Linode, d *schema.ResourceData) error {
	updates := make(map[string]interface{})
	if d.Get("group").(string) != linode.LpmDisplayGroup {
		updates["lpm_displayGroup"] = d.Get("group")
	}

	if d.Get("name").(string) != linode.Label.String() {
		updates["Label"] = d.Get("name")
	}

	if len(updates) > 0 {
		_, err := client.Linode.Update(linode.LinodeId, updates)
		if err != nil {
			return fmt.Errorf("Failed to update the linode's group because %s", err)
		}
	}
	d.SetPartial("group")
	d.SetPartial("name")
	return nil
}

// changeLinodeSize resizes the current linode
func changeLinodeSize(client *linodego.Client, linode linodego.Linode, d *schema.ResourceData) error {
	var newPlanID int
	var waitMinutes int

	// Get the Linode Plan Size
	sizeId, err := getSizeId(client, d.Get("size").(int))
	if err != nil {
		return fmt.Errorf("Failed to find a Plan with %d RAM because %s", d.Get("size"), err)
	}
	newPlanID = sizeId

	// Check if we can safely resize, with Disk Size considered
	currentDiskSize, err := getTotalDiskSize(client, linode.LinodeId)
	newDiskSize, err := getPlanDiskSize(client, newPlanID)
	if currentDiskSize > newDiskSize {
		return fmt.Errorf("Cannot resize linode %d because currentDisk(%d GB) is bigger than newDisk(%d GB)", linode.LinodeId, currentDiskSize, newDiskSize)
	}

	// Resize the Linode
	client.Linode.Resize(linode.LinodeId, newPlanID)
	// Linode says 1-3 minutes per gigabyte for Resize time... Let's be safe with 3
	waitMinutes = ((linode.TotalHD / 1024) * 3)
	// Wait for the Resize Operation to Complete
	err = waitForJobsToCompleteWaitMinutes(client, linode.LinodeId, waitMinutes)
	if err != nil {
		return fmt.Errorf("Failed to wait for linode %d resize because %s", linode.LinodeId, err)
	}

	if d.Get("disk_expansion").(bool) {
		// Determine the biggestDisk ID and Size
		biggestDiskID, biggestDiskSize, err := getBiggestDisk(client, linode.LinodeId)
		if err != nil {
			return err
		}
		// Calculate new size, with other disks taken into consideration
		expandedDiskSize := (newDiskSize - (currentDiskSize - biggestDiskSize))

		// Resize the Disk
		client.Disk.Resize(linode.LinodeId, biggestDiskID, expandedDiskSize)
		// Wait for the Disk Resize Operation to Complete
		err = waitForJobsToCompleteWaitMinutes(client, linode.LinodeId, waitMinutes)
		if err != nil {
			return fmt.Errorf("Failed to wait for resize of Disk %d for Linode %d because %s", biggestDiskID, linode.LinodeId, err)
		}
	}

	// Boot up the resized Linode
	client.Linode.Boot(linode.LinodeId, 0)

	// Return the new Linode size
	d.SetPartial("size")
	return nil
}

// changeLinodeConfig changes Config level settings. This is things like the various helpers
func changeLinodeConfig(client *linodego.Client, config linodego.LinodeConfig, d *schema.ResourceData) error {
	updates := make(map[string]string)
	if d.HasChange("helper_distro") {
		updates["helper_distro"] = boolToString(d.Get("helper_distro").(bool))
	}
	if d.HasChange("manage_private_ip_automatically") {
		updates["helper_network"] = boolToString(d.Get("manage_private_ip_automatically").(bool))
	}

	if len(updates) > 0 {
		_, err := client.Config.Update(config.ConfigId, config.LinodeId, config.KernelId, updates)
		if err != nil {
			return fmt.Errorf("Failed to update the linode's config because %s", err)
		}
	}
	d.SetPartial("helper_distro")
	d.SetPartial("manage_private_ip_automatically")
	return nil
}

// Converts a bool to a string
func boolToString(val bool) string {
	if val {
		return "true"
	}
	return "false"
}
