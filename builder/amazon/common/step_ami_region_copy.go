package common

import (
	"context"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/hashicorp/packer/helper/multistep"
	"github.com/hashicorp/packer/packer"
)

type StepAMIRegionCopy struct {
	AccessConfig      *AccessConfig
	Regions           []string
	AMIKmsKeyId       string
	RegionKeyIds      map[string]string
	EncryptBootVolume *bool // nil means preserve
	Name              string

	toDelete string
}

func (s *StepAMIRegionCopy) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	ec2conn := state.Get("ec2").(*ec2.EC2)
	ui := state.Get("ui").(packer.Ui)
	amis := state.Get("amis").(map[string]string)
	snapshots := state.Get("snapshots").(map[string][]string)
	ami := amis[*ec2conn.Config.Region]

	if s.EncryptBootVolume != nil {
		// encrypt_boot was set, we now have to copy the temporary
		// AMI with required encryption setting.
		// temp image was created by stepCreateAMI.
		s.Regions = append(s.Regions, *ec2conn.Config.Region)
		if s.RegionKeyIds == nil {
			s.RegionKeyIds = make(map[string]string)
		}
		s.RegionKeyIds[*ec2conn.Config.Region] = s.AMIKmsKeyId
		s.toDelete = ami
	}

	if len(s.Regions) == 0 {
		return multistep.ActionContinue
	}

	ui.Say(fmt.Sprintf("Copying AMI (%s) to other regions...", ami))

	var lock sync.Mutex
	var wg sync.WaitGroup
	var regKeyID string
	errs := new(packer.MultiError)

	wg.Add(len(s.Regions))
	for _, region := range s.Regions {
		if region == *ec2conn.Config.Region &&
			(s.EncryptBootVolume == nil || *s.EncryptBootVolume == false) {
			ui.Message(fmt.Sprintf(
				"Avoiding copying AMI to duplicate region %s", region))
			wg.Done()
			continue
		}

		ui.Message(fmt.Sprintf("Copying to: %s", region))

		if s.EncryptBootVolume != nil && *s.EncryptBootVolume {
			regKeyID = s.RegionKeyIds[region]
		}

		go func(region string) {
			defer wg.Done()
			id, snapshotIds, err := amiRegionCopy(ctx, state, s.AccessConfig, s.Name, ami, region, *ec2conn.Config.Region, regKeyID, s.EncryptBootVolume)
			lock.Lock()
			defer lock.Unlock()
			amis[region] = id
			snapshots[region] = snapshotIds
			if err != nil {
				errs = packer.MultiErrorAppend(errs, err)
			}
		}(region)
	}

	// TODO(mitchellh): Wait but also allow for cancels to go through...
	ui.Message("Waiting for all copies to complete...")
	wg.Wait()

	// If there were errors, show them
	if len(errs.Errors) > 0 {
		state.Put("error", errs)
		ui.Error(errs.Error())
		return multistep.ActionHalt
	}

	state.Put("amis", amis)
	return multistep.ActionContinue
}

func (s *StepAMIRegionCopy) Cleanup(state multistep.StateBag) {
	ec2conn := state.Get("ec2").(*ec2.EC2)
	ui := state.Get("ui").(packer.Ui)

	// Delete the unencrypted amis and snapshots
	ui.Say("Deregistering the AMI and deleting unencrypted temporary " +
		"AMIs and snapshots")

	resp, err := ec2conn.DescribeImages(&ec2.DescribeImagesInput{
		ImageIds: []*string{&s.toDelete},
	})

	if err != nil {
		err := fmt.Errorf("Error describing AMI: %s", err)
		state.Put("error", err)
		ui.Error(err.Error())
		return
	}

	// Deregister image by name.
	for _, i := range resp.Images {
		_, err := ec2conn.DeregisterImage(&ec2.DeregisterImageInput{
			ImageId: i.ImageId,
		})

		if err != nil {
			err := fmt.Errorf("Error deregistering existing AMI: %s", err)
			state.Put("error", err)
			ui.Error(err.Error())
			return
		}
		ui.Say(fmt.Sprintf("Deregistered AMI id: %s", *i.ImageId))

		// Delete snapshot(s) by image
		for _, b := range i.BlockDeviceMappings {
			if b.Ebs != nil && aws.StringValue(b.Ebs.SnapshotId) != "" {
				_, err := ec2conn.DeleteSnapshot(&ec2.DeleteSnapshotInput{
					SnapshotId: b.Ebs.SnapshotId,
				})

				if err != nil {
					err := fmt.Errorf("Error deleting existing snapshot: %s", err)
					state.Put("error", err)
					ui.Error(err.Error())
					return
				}
				ui.Say(fmt.Sprintf("Deleted snapshot: %s", *b.Ebs.SnapshotId))
			}
		}
	}
}

// amiRegionCopy does a copy for the given AMI to the target region and
// returns the resulting ID and snapshot IDs, or error.
func amiRegionCopy(ctx context.Context, state multistep.StateBag, config *AccessConfig, name, imageId,
	target, source, keyId string, encrypt *bool) (string, []string, error) {
	snapshotIds := []string{}

	// Connect to the region where the AMI will be copied to
	session, err := config.Session()
	if err != nil {
		return "", snapshotIds, err
	}

	regionconn := ec2.New(session.Copy(&aws.Config{
		Region: aws.String(target),
	}))

	resp, err := regionconn.CopyImage(&ec2.CopyImageInput{
		SourceRegion:  &source,
		SourceImageId: &imageId,
		Name:          &name,
		Encrypted:     encrypt,
		KmsKeyId:      aws.String(keyId),
	})

	if err != nil {
		return "", snapshotIds, fmt.Errorf("Error Copying AMI (%s) to region (%s): %s",
			imageId, target, err)
	}

	// Wait for the image to become ready
	if err := WaitUntilAMIAvailable(ctx, regionconn, *resp.ImageId); err != nil {
		return "", snapshotIds, fmt.Errorf("Error waiting for AMI (%s) in region (%s): %s",
			*resp.ImageId, target, err)
	}

	// Getting snapshot IDs out of the copied AMI
	describeImageResp, err := regionconn.DescribeImages(&ec2.DescribeImagesInput{ImageIds: []*string{resp.ImageId}})
	if err != nil {
		return "", snapshotIds, fmt.Errorf("Error describing copied AMI (%s) in region (%s): %s",
			imageId, target, err)
	}

	for _, blockDeviceMapping := range describeImageResp.Images[0].BlockDeviceMappings {
		if blockDeviceMapping.Ebs != nil && blockDeviceMapping.Ebs.SnapshotId != nil {
			snapshotIds = append(snapshotIds, *blockDeviceMapping.Ebs.SnapshotId)
		}
	}

	return *resp.ImageId, snapshotIds, nil
}
