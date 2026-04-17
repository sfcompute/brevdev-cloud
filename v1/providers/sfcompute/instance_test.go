package v1

import (
	"encoding/base64"
	"strings"
	"testing"

	v1 "github.com/brevdev/cloud/v1"
	"github.com/stretchr/testify/require"
)

func TestInstanceMetadataFromNodePrefersManagedTags(t *testing.T) {
	t.Parallel()

	client := &SFCClient{refID: "fallback-ref"}
	tags := v1.Tags{
		tagManagedBy:      tagManagedByValue,
		tagCloudCredRefID: "cloud-cred-ref",
		tagRefID:          "instance-ref",
		tagInstanceName:   "instance-name",
	}

	cloudCredRefID, refID, name, err := client.instanceMetadataFromNode("legacy_name", tags)
	require.NoError(t, err)
	require.Equal(t, "cloud-cred-ref", cloudCredRefID)
	require.Equal(t, "instance-ref", refID)
	require.Equal(t, "instance-name", name)
}

func TestSFCStatusToLifecycleStatus(t *testing.T) {
	t.Parallel()

	cases := map[string]v1.LifecycleStatus{
		"awaiting_allocation": v1.LifecycleStatusPending,
		"running":             v1.LifecycleStatusRunning,
		"terminated":          v1.LifecycleStatusTerminated,
		"failed":              v1.LifecycleStatusFailed,
	}

	for input, expected := range cases {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, expected, sfcStatusToLifecycleStatus(input))
		})
	}
}

func TestCloudInitUserDataForCreateUsesShellScript(t *testing.T) {
	t.Parallel()

	userData := cloudInitUserDataForCreate(v1.CreateInstanceAttrs{
		PublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest user@example.com",
	})
	require.NotNil(t, userData)

	decoded, err := base64.StdEncoding.DecodeString(*userData)
	require.NoError(t, err)

	script := string(decoded)
	require.True(t, strings.HasPrefix(script, "#!/bin/bash"))
	require.Contains(t, script, "/root/.ssh/authorized_keys")
	require.Contains(t, script, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest user@example.com")
}
