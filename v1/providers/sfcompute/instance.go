package v1

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/alecthomas/units"
	"github.com/brevdev/cloud/internal/errors"
	v1 "github.com/brevdev/cloud/v1"
	sfc "github.com/sfcompute/sfc-go"
	"github.com/sfcompute/sfc-go/models/components"
	"github.com/sfcompute/sfc-go/models/operations"
	"github.com/sfcompute/sfc-go/optionalnullable"
)

const (
	defaultSSHPort                      = 22
	defaultSSHUsername                  = "root"
	defaultImageName                    = "ubuntu-22.04.5-cuda-12.7"
	defaultManagedWindowMinutes         = 60
	defaultMinSellPriceDollarsPerNodeHr = "0.010000"

	tagManagedBy       = "brev-managed-by"
	tagManagedByValue  = "brev-cloud-sfcompute"
	tagCloudCredRefID  = "brev-cloud-cred-ref-id"
	tagStage           = "brev-stage"
	tagRefID           = "brev-ref-id"
	tagInstanceName    = "brev-instance-name"
	tagLocation        = "brev-location"
	tagInstanceType    = "brev-instance-type"
)

func (c *SFCClient) CreateInstance(ctx context.Context, attrs v1.CreateInstanceAttrs) (*v1.Instance, error) {
	if existing, err := c.findManagedInstanceByRefID(ctx, attrs.RefID); err == nil && existing != nil {
		return existing, nil
	}

	location := attrs.Location
	if location == "" {
		location = c.location
	}
	if location == "" {
		return nil, errors.WrapAndTrace(fmt.Errorf("location is required"))
	}

	zone, err := c.getZone(ctx, location, false)
	if err != nil {
		return nil, errors.WrapAndTrace(err)
	}

	selectedType, err := getInstanceTypeForZone(*zone)
	if err != nil {
		return nil, errors.WrapAndTrace(err)
	}
	if attrs.InstanceType != "" && attrs.InstanceType != selectedType.Type {
		return nil, errors.WrapAndTrace(fmt.Errorf("instance type %q is not available in zone %q", attrs.InstanceType, zone.Name))
	}

	workspace, err := c.getDefaultWorkspace(ctx)
	if err != nil {
		return nil, errors.WrapAndTrace(err)
	}

	imageID, err := c.resolveImageID(ctx, attrs.ImageID)
	if err != nil {
		return nil, errors.WrapAndTrace(err)
	}

	stage := getStageFromTags(attrs.Tags)
	nodeName := brevDataToSFCName(c.refID, stage, attrs.RefID, attrs.Name)
	nodeTags := makeManagedNodeTags(c.refID, stage, attrs, location, selectedType.Type)
	resourceTags := makeManagedResourceTags(c.refID, stage, attrs.RefID, attrs.Name, location, selectedType.Type)

	capacityName := makeManagedPoolName("brev-cap", c.refID, stage, location)
	procurementName := makeManagedPoolName("brev-proc", c.refID, stage, location)

	var createdCapacityID string
	var createdProcurementID string
	var createdNodeID string
	cleanup := true
	defer func() {
		if cleanup {
			c.cleanupFailedInstanceCreate(ctx, createdNodeID, createdProcurementID, createdCapacityID)
		}
	}()

	capacityID, procurementID, createdCapacity, createdProcurement, err := c.ensureManagedCapacityAndProcurement(
		ctx,
		workspace,
		zone.Name,
		stage,
		selectedType.Type,
		resourceTags,
		capacityName,
		procurementName,
		selectedType.BasePrice.Number(),
	)
	if err != nil {
		return nil, errors.WrapAndTrace(err)
	}
	if createdCapacity {
		createdCapacityID = capacityID
	}
	if createdProcurement {
		createdProcurementID = procurementID
	}

	nodeReq := components.CreateNodeRequest{
		Name:     optionalnullable.From(&nodeName),
		Capacity: capacityID,
		Image:    imageID,
		Tags:     optionalnullable.From(&nodeTags),
	}
	if userData := cloudInitUserDataForCreate(attrs); userData != nil {
		nodeReq.CloudInitUserData = userData
	}

	nodeResp, err := c.client.Nodes.Create(ctx, nodeReq)
	if err != nil {
		return nil, errors.WrapAndTrace(err)
	}
	node := nodeResp.GetNodeResponse()
	if node == nil {
		return nil, errors.WrapAndTrace(fmt.Errorf("node response missing node"))
	}
	createdNodeID = node.ID

	instance, err := c.GetInstance(ctx, v1.CloudProviderInstanceID(node.ID))
	if err != nil {
		return nil, errors.WrapAndTrace(err)
	}

	cleanup = false
	return instance, nil
}

func cloudInitUserDataForCreate(attrs v1.CreateInstanceAttrs) *string {
	if attrs.UserDataBase64 != "" {
		return &attrs.UserDataBase64
	}
	if attrs.PublicKey == "" {
		return nil
	}

	script := fmt.Sprintf(`#!/bin/bash
set -e
mkdir -p /root/.ssh
chmod 700 /root/.ssh
cat >>/root/.ssh/authorized_keys <<'EOF'
%s
EOF
chmod 600 /root/.ssh/authorized_keys
`, strings.TrimSpace(attrs.PublicKey))
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	return &encoded
}

func (c *SFCClient) GetInstance(ctx context.Context, id v1.CloudProviderInstanceID) (*v1.Instance, error) {
	c.logger.Debug(ctx, "sfc: GetInstance start",
		v1.LogField("instanceID", id),
		v1.LogField("location", c.location),
	)

	resp, err := c.client.Nodes.Fetch(ctx, string(id), nil)
	if err != nil {
		return nil, errors.WrapAndTrace(err)
	}
	node := resp.GetNodeResponse()
	if node == nil {
		return nil, errors.WrapAndTrace(fmt.Errorf("node response missing node"))
	}

	nodeInfo, err := c.sfcNodeInfoFromNode(ctx, node)
	if err != nil {
		return nil, errors.WrapAndTrace(err)
	}

	instance, err := c.sfcNodeToBrevInstance(*nodeInfo)
	if err != nil {
		return nil, errors.WrapAndTrace(err)
	}

	c.logger.Debug(ctx, "sfc: GetInstance end",
		v1.LogField("instanceID", id),
		v1.LogField("instance", instance),
	)

	return instance, nil
}

func (c *SFCClient) ListInstances(ctx context.Context, args v1.ListInstancesArgs) ([]v1.Instance, error) {
	c.logger.Debug(ctx, "sfc: ListInstances start",
		v1.LogField("location", c.location),
		v1.LogField("args", fmt.Sprintf("%+v", args)),
	)

	req := operations.ListNodesRequest{
		Limit: sfc.Int64(200),
	}
	if len(args.InstanceIDs) > 0 {
		req.ID = make([]string, 0, len(args.InstanceIDs))
		for _, id := range args.InstanceIDs {
			req.ID = append(req.ID, string(id))
		}
	}

	resp, err := c.client.Nodes.List(ctx, req)
	if err != nil {
		return nil, errors.WrapAndTrace(err)
	}

	var instances []v1.Instance
	for resp != nil {
		list := resp.GetListNodesResponse()
		if list == nil {
			break
		}

		for _, node := range list.Data {
			nodeInfo, err := c.sfcNodeInfoFromNode(ctx, &node)
			if err != nil {
				c.logger.Error(ctx, err,
					v1.LogField("msg", "sfc: ListInstances skipping node due to error"),
					v1.LogField("nodeID", node.ID),
					v1.LogField("nodeName", node.Name),
				)
				continue
			}

			inst, err := c.sfcNodeToBrevInstance(*nodeInfo)
			if err != nil {
				c.logger.Error(ctx, err,
					v1.LogField("msg", "sfc: ListInstances skipping node due to conversion error"),
					v1.LogField("nodeID", node.ID),
					v1.LogField("nodeName", node.Name),
				)
				continue
			}

			if args.Locations != nil && !args.Locations.IsAllowed(inst.Location) {
				continue
			}
			if len(args.TagFilters) > 0 && !matchesTagFilters(inst.Tags, args.TagFilters) {
				continue
			}

			instances = append(instances, *inst)
		}

		if !list.HasMore || resp.Next == nil {
			break
		}
		resp, err = resp.Next()
		if err != nil {
			return nil, errors.WrapAndTrace(err)
		}
	}

	c.logger.Debug(ctx, "sfc: ListInstances end",
		v1.LogField("instance count", len(instances)),
	)

	return instances, nil
}

func (c *SFCClient) TerminateInstance(ctx context.Context, id v1.CloudProviderInstanceID) error {
	c.logger.Debug(ctx, "sfc: TerminateInstance start",
		v1.LogField("instanceID", id),
	)

	_, err := c.client.Nodes.TerminateNode(ctx, string(id))
	if err != nil {
		return errors.WrapAndTrace(err)
	}

	c.logger.Debug(ctx, "sfc: TerminateInstance end",
		v1.LogField("instanceID", id),
	)

	return nil
}

type sfcNodeInfo struct {
	id             string
	name           string
	refID          string
	cloudCredRefID string
	location       string
	instanceType   string
	createdAt      time.Time
	status         v1.LifecycleStatus
	sshUsername    string
	sshHostname    string
	sshPort        int
	tags           v1.Tags
}

func (c *SFCClient) sfcNodeToBrevInstance(node sfcNodeInfo) (*v1.Instance, error) {
	instanceType, err := getInstanceTypeForLocationAndType(node.location, node.instanceType)
	if err != nil {
		return nil, errors.WrapAndTrace(err)
	}

	diskSizeInt64, err := instanceType.SupportedStorage[0].SizeBytes.ByteCountInUnitInt64(v1.Gibibyte)
	if err != nil {
		return nil, err
	}
	diskSize := units.Base2Bytes(diskSizeInt64 * int64(units.Gibibyte))

	inst := &v1.Instance{
		Name:          node.name,
		CloudID:       v1.CloudProviderInstanceID(node.id),
		RefID:         node.refID,
		PublicDNS:     node.sshHostname,
		PublicIP:      node.sshHostname,
		SSHUser:       node.sshUsername,
		SSHPort:       node.sshPort,
		CreatedAt:     node.createdAt,
		DiskSize:      diskSize,
		DiskSizeBytes: instanceType.SupportedStorage[0].SizeBytes,
		Status: v1.Status{
			LifecycleStatus: node.status,
		},
		InstanceTypeID: instanceType.ID,
		InstanceType:   instanceType.Type,
		Location:       node.location,
		Spot:           false,
		Stoppable:      false,
		Rebootable:     false,
		CloudCredRefID: node.cloudCredRefID,
		Tags:           node.tags,
	}
	return inst, nil
}

func (c *SFCClient) sfcNodeInfoFromNode(ctx context.Context, node *components.NodeResponse) (*sfcNodeInfo, error) {
	tags := optionalTagsToMap(node.Tags)

	cloudCredRefID, refID, name, err := c.instanceMetadataFromNode(node.Name, tags)
	if err != nil {
		return nil, err
	}

	location, err := c.locationFromNode(node, tags)
	if err != nil {
		return nil, err
	}

	instanceType, err := c.instanceTypeFromNode(ctx, node, tags, location)
	if err != nil {
		return nil, err
	}

	status := sfcStatusToLifecycleStatus(string(node.Status))
	sshHostname := ""
	sshPort := defaultSSHPort
	if status == v1.LifecycleStatusRunning {
		hostname, port, ok := c.getSSHInfoForNode(ctx, node.ID)
		if ok {
			sshHostname = hostname
			sshPort = port
		} else {
			// The v2 API reports nodes as running before the VM has fully booted.
			// Treat the instance as pending until SSH information is available.
			status = v1.LifecycleStatusPending
		}
	}

	return &sfcNodeInfo{
		id:             node.ID,
		name:           name,
		refID:          refID,
		cloudCredRefID: cloudCredRefID,
		location:       location,
		instanceType:   instanceType,
		createdAt:      time.Unix(node.CreatedAt, 0),
		status:         status,
		sshUsername:    defaultSSHUsername,
		sshHostname:    sshHostname,
		sshPort:        sshPort,
		tags:           tags,
	}, nil
}

func (c *SFCClient) getSSHInfoForNode(ctx context.Context, nodeID string) (string, int, bool) {
	resp, err := c.client.Nodes.GetSSHInfoForNode(ctx, nodeID)
	if err != nil {
		c.logger.Debug(ctx, "sfc: SSH info unavailable yet",
			v1.LogField("nodeID", nodeID),
			v1.LogField("error", err.Error()),
		)
		return "", 0, false
	}

	info := resp.GetNodeSSHInfo()
	if info == nil || info.Hostname == "" {
		return "", 0, false
	}

	port := int(info.Port)
	if port == 0 {
		port = defaultSSHPort
	}
	return info.Hostname, port, true
}

func (c *SFCClient) locationFromNode(node *components.NodeResponse, tags v1.Tags) (string, error) {
	if location := tags[tagLocation]; location != "" {
		return location, nil
	}
	if zone, ok := optionalStringValue(node.Zone); ok && zone != "" {
		return zone, nil
	}
	if c.location != "" {
		return c.location, nil
	}
	return "", fmt.Errorf("node %s missing location metadata", node.ID)
}

func (c *SFCClient) instanceTypeFromNode(ctx context.Context, node *components.NodeResponse, tags v1.Tags, location string) (string, error) {
	if instanceType := tags[tagInstanceType]; instanceType != "" {
		return instanceType, nil
	}

	zoneName := location
	if zone, ok := optionalStringValue(node.Zone); ok && zone != "" {
		zoneName = zone
	}

	zone, err := c.getZone(ctx, zoneName, true)
	if err != nil {
		return "", err
	}
	return makeInstanceTypeName(*zone), nil
}

func (c *SFCClient) instanceMetadataFromNode(nodeName string, tags v1.Tags) (string, string, string, error) {
	if tags[tagManagedBy] == tagManagedByValue && tags[tagRefID] != "" {
		cloudCredRefID := tags[tagCloudCredRefID]
		if cloudCredRefID == "" {
			cloudCredRefID = c.refID
		}
		return cloudCredRefID, tags[tagRefID], tags[tagInstanceName], nil
	}

	cloudCredRefID, _, refID, name, err := sfcNameToBrevData(nodeName)
	if err != nil {
		return "", "", "", err
	}
	if cloudCredRefID == "" {
		cloudCredRefID = c.refID
	}
	return cloudCredRefID, refID, name, nil
}

func (c *SFCClient) resolveImageID(ctx context.Context, requested string) (string, error) {
	if strings.HasPrefix(requested, "image_") || strings.HasPrefix(requested, "sfc:image:") {
		return requested, nil
	}

	targetName := requested
	if targetName == "" {
		targetName = defaultImageName
	}

	req := operations.ListImagesRequest{
		Limit: sfc.Int64(200),
	}
	resp, err := c.client.Images.List(ctx, req)
	if err != nil {
		return "", err
	}

	var fallbackImageID string
	for resp != nil {
		list := resp.GetListImagesResponse()
		if list == nil {
			break
		}

		for _, image := range list.Data {
			if image.UploadStatus != components.ImageUploadStatusCompleted {
				continue
			}
			if fallbackImageID == "" {
				fallbackImageID = image.ID
			}
			if image.ID == requested || image.ResourcePath == requested || image.Name == targetName {
				return image.ID, nil
			}
		}

		if !list.HasMore || resp.Next == nil {
			break
		}
		resp, err = resp.Next()
		if err != nil {
			return "", err
		}
	}

	if requested != "" {
		return "", fmt.Errorf("image %q not found", requested)
	}
	if fallbackImageID != "" {
		return fallbackImageID, nil
	}
	return "", fmt.Errorf("no completed SF Compute images available")
}

func (c *SFCClient) findManagedInstanceByRefID(ctx context.Context, refID string) (*v1.Instance, error) {
	if refID == "" {
		return nil, nil
	}

	resp, err := c.client.Nodes.List(ctx, operations.ListNodesRequest{
		Limit: sfc.Int64(50),
		Tag: []string{
			fmt.Sprintf("%s=%s", tagManagedBy, tagManagedByValue),
			fmt.Sprintf("%s=%s", tagRefID, refID),
		},
	})
	if err != nil {
		return nil, err
	}
	for resp != nil {
		list := resp.GetListNodesResponse()
		if list == nil {
			break
		}

		for _, node := range list.Data {
			nodeInfo, err := c.sfcNodeInfoFromNode(ctx, &node)
			if err != nil {
				return nil, err
			}
			if nodeInfo.status == v1.LifecycleStatusTerminated {
				continue
			}
			return c.sfcNodeToBrevInstance(*nodeInfo)
		}

		if !list.HasMore || resp.Next == nil {
			break
		}
		resp, err = resp.Next()
		if err != nil {
			return nil, err
		}
	}

	return nil, nil
}

func (c *SFCClient) cleanupFailedInstanceCreate(ctx context.Context, nodeID string, procurementID string, capacityID string) {
	if nodeID != "" {
		if _, err := c.client.Nodes.TerminateNode(ctx, nodeID); err != nil {
			c.logger.Error(ctx, err, v1.LogField("msg", "sfc: failed to clean up node after create failure"), v1.LogField("nodeID", nodeID))
		}
	}
	if procurementID != "" {
		if _, err := c.client.Procurements.Delete(ctx, procurementID); err != nil {
			c.logger.Error(ctx, err, v1.LogField("msg", "sfc: failed to clean up procurement after create failure"), v1.LogField("procurementID", procurementID))
		}
	}
	if capacityID != "" {
		if _, err := c.client.Capacities.Delete(ctx, capacityID); err != nil {
			c.logger.Error(ctx, err, v1.LogField("msg", "sfc: failed to clean up capacity after create failure"), v1.LogField("capacityID", capacityID))
		}
	}
}

func makeManagedNodeTags(cloudCredRefID string, stage string, attrs v1.CreateInstanceAttrs, location string, instanceType string) map[string]string {
	tags := make(map[string]string, len(attrs.Tags)+7)
	for k, v := range attrs.Tags {
		tags[k] = v
	}

	tags[tagManagedBy] = tagManagedByValue
	tags[tagCloudCredRefID] = cloudCredRefID
	tags[tagStage] = stage
	tags[tagRefID] = attrs.RefID
	tags[tagInstanceName] = attrs.Name
	tags[tagLocation] = location
	tags[tagInstanceType] = instanceType
	return tags
}

func makeManagedResourceTags(cloudCredRefID string, stage string, refID string, name string, location string, instanceType string) map[string]string {
	return map[string]string{
		tagManagedBy:      tagManagedByValue,
		tagCloudCredRefID: cloudCredRefID,
		tagStage:          stage,
		tagRefID:          refID,
		tagInstanceName:   name,
		tagLocation:       location,
		tagInstanceType:   instanceType,
	}
}

func makeManagedPoolName(prefix string, cloudCredRefID string, stage string, location string) string {
	sanitized := sanitizeManagedNamePart(fmt.Sprintf("%s-%s-%s", cloudCredRefID, stage, location))
	return fmt.Sprintf("%s-%s", prefix, sanitized)
}

func sanitizeManagedNamePart(value string) string {
	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, value)
	sanitized = strings.TrimLeft(sanitized, "-_.")
	if sanitized == "" {
		sanitized = "default"
	}
	return sanitized
}

func optionalTagsToMap(tags optionalnullable.OptionalNullable[map[string]string]) v1.Tags {
	if tags == nil {
		return nil
	}
	value, ok := tags.GetOrZero()
	if !ok || tags.IsNull() {
		return nil
	}

	copied := make(v1.Tags, len(value))
	for k, v := range value {
		copied[k] = v
	}
	return copied
}

func optionalStringValue(value optionalnullable.OptionalNullable[string]) (string, bool) {
	if value == nil {
		return "", false
	}
	s, ok := value.GetOrZero()
	if !ok || value.IsNull() {
		return "", false
	}
	return s, true
}

// sfcStatusToLifecycleStatus maps v2 node statuses to Brev lifecycle statuses.
func sfcStatusToLifecycleStatus(status string) v1.LifecycleStatus {
	switch strings.ToLower(status) {
	case "awaiting_allocation", "pending", "unspecified", "unknown":
		return v1.LifecycleStatusPending
	case "running":
		return v1.LifecycleStatusRunning
	case "stopped":
		return v1.LifecycleStatusStopped
	case "terminating":
		return v1.LifecycleStatusTerminating
	case "terminated", "released", "destroyed", "deleted":
		return v1.LifecycleStatusTerminated
	case "nodefailure", "failed":
		return v1.LifecycleStatusFailed
	default:
		return v1.LifecycleStatusPending
	}
}

func matchesTagFilters(instanceTags map[string]string, tagFilters map[string][]string) bool {
	for filterKey, acceptableValues := range tagFilters {
		instanceValue, hasTag := instanceTags[filterKey]
		if !hasTag {
			return false
		}
		if !slices.Contains(acceptableValues, instanceValue) {
			return false
		}
	}
	return true
}

// brevDataToSFCName packs cloud credential ref ID, brev stage, instance ref ID, and instance
// name into a single SFC node name, separated by underscores. This is necessary because SFC
// has no tags/labels API — the node name is the only place to store metadata.
//
// Format: {cloudCredRefID}_{brevStage}_{refID}_{name}
func brevDataToSFCName(cloudCredRefID string, brevStage string, refID string, name string) string {
	return fmt.Sprintf("%s_%s_%s_%s", cloudCredRefID, brevStage, refID, name)
}

// sfcNameToBrevData parses an SFC node name back into its components.
//
// Supports two formats for backward compatibility:
//   - New (4+ parts): {cloudCredRefID}_{brevStage}_{refID}_{name}
//   - Old (2 parts): {refID}_{name} — cloudCredRefID and brevStage returned empty
func sfcNameToBrevData(name string) (cloudCredRefID string, brevStage string, refID string, instanceName string, err error) {
	parts := strings.SplitN(name, "_", 4)
	switch len(parts) {
	case 4:
		// New format: cloudCredRefID_brevStage_refID_name
		return parts[0], parts[1], parts[2], parts[3], nil
	case 2:
		// Old format: refID_name (backward compat — cloudCredRefID and stage unknown)
		// TODO: remove this case once all old-format nodes have been cleaned up
		return "", "", parts[0], parts[1], nil
	default:
		return "", "", "", "", errors.WrapAndTrace(fmt.Errorf("invalid node name %s: expected 2 or 4 underscore-separated parts", name))
	}
}

// getStageFromTags extracts the control plane stage value from instance tags.
// The tag key is prefixed by the control plane
// so we match any key ending with "-stage" to avoid coupling to a specific prefix.
func getStageFromTags(tags v1.Tags) string {
	for k, v := range tags {
		if strings.HasSuffix(k, "-stage") {
			return v
		}
	}
	return "unknown"
}

func (c *SFCClient) ensureManagedCapacityAndProcurement(
	ctx context.Context,
	workspace string,
	location string,
	stage string,
	instanceType string,
	resourceTags map[string]string,
	capacityName string,
	procurementName string,
	maxBuyPrice string,
) (capacityID string, procurementID string, createdCapacity bool, createdProcurement bool, err error) {
	capacityID, err = c.findManagedCapacityID(ctx, stage, location, instanceType)
	if err != nil {
		return "", "", false, false, err
	}

	if capacityID == "" {
		capacityResp, err := c.client.Capacities.Create(ctx, components.CreateCapacityRequest{
			Name:      optionalnullable.From(&capacityName),
			Workspace: workspace,
			Zones:     []string{location},
			Tags:      optionalnullable.From(&resourceTags),
		})
		if err != nil {
			return "", "", false, false, err
		}
		capacity := capacityResp.GetCapacityResponse()
		if capacity == nil {
			return "", "", false, false, fmt.Errorf("capacity response missing capacity")
		}
		capacityID = capacity.ID
		createdCapacity = true
	}

	procurementID, err = c.findManagedProcurementID(ctx, capacityID)
	if err != nil {
		return "", "", createdCapacity, false, err
	}
	if procurementID != "" {
		return capacityID, procurementID, createdCapacity, false, nil
	}

	enabled := true
	procurementResp, err := c.client.Procurements.Create(ctx, components.CreateProcurementRequest{
		Name:                           optionalnullable.From(&procurementName),
		Target:                         components.CreateProcurementTargetNodeCountTag("node_count"),
		Capacity:                       capacityID,
		MinSellPriceDollarsPerNodeHour: defaultMinSellPriceDollarsPerNodeHr,
		MaxBuyPriceDollarsPerNodeHour:  maxBuyPrice,
		ManagedWindowMinutes:           defaultManagedWindowMinutes,
		Enabled:                        &enabled,
	})
	if err != nil {
		return "", "", createdCapacity, false, err
	}
	procurement := procurementResp.GetProcurementResponse()
	if procurement == nil {
		return "", "", createdCapacity, false, fmt.Errorf("procurement response missing procurement")
	}

	return capacityID, procurement.ID, createdCapacity, true, nil
}

func (c *SFCClient) findManagedCapacityID(ctx context.Context, stage string, location string, instanceType string) (string, error) {
	resp, err := c.client.Capacities.List(ctx, operations.ListCapacitiesRequest{
		Limit: sfc.Int64(100),
		Tag: []string{
			fmt.Sprintf("%s=%s", tagManagedBy, tagManagedByValue),
			fmt.Sprintf("%s=%s", tagCloudCredRefID, c.refID),
			fmt.Sprintf("%s=%s", tagStage, stage),
			fmt.Sprintf("%s=%s", tagLocation, location),
			fmt.Sprintf("%s=%s", tagInstanceType, instanceType),
		},
	})
	if err != nil {
		return "", err
	}

	for resp != nil {
		list := resp.GetListCapacitiesResponse()
		if list == nil {
			break
		}
		if len(list.Data) > 0 {
			return list.Data[0].ID, nil
		}
		if !list.HasMore || resp.Next == nil {
			break
		}
		resp, err = resp.Next()
		if err != nil {
			return "", err
		}
	}

	return "", nil
}

func (c *SFCClient) findManagedProcurementID(ctx context.Context, capacityID string) (string, error) {
	resp, err := c.client.Procurements.List(ctx, operations.ListProcurementsRequest{
		Capacity: &capacityID,
		Limit:    sfc.Int64(100),
	})
	if err != nil {
		return "", err
	}

	for resp != nil {
		list := resp.GetListProcurementsResponse()
		if list == nil {
			break
		}
		if len(list.Data) > 0 {
			return list.Data[0].ID, nil
		}
		if !list.HasMore || resp.Next == nil {
			break
		}
		resp, err = resp.Next()
		if err != nil {
			return "", err
		}
	}

	return "", nil
}

// Optional if supported:
func (c *SFCClient) RebootInstance(_ context.Context, _ v1.CloudProviderInstanceID) error {
	return v1.ErrNotImplemented
}

func (c *SFCClient) StopInstance(_ context.Context, _ v1.CloudProviderInstanceID) error {
	return v1.ErrNotImplemented
}

func (c *SFCClient) StartInstance(_ context.Context, _ v1.CloudProviderInstanceID) error {
	return v1.ErrNotImplemented
}

// Merge strategies (pass-through is acceptable baseline).
func (c *SFCClient) MergeInstanceForUpdate(_ v1.Instance, newInst v1.Instance) v1.Instance {
	return newInst
}

func (c *SFCClient) MergeInstanceTypeForUpdate(_ v1.InstanceType, newIt v1.InstanceType) v1.InstanceType {
	return newIt
}
