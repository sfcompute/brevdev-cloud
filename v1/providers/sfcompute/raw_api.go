package v1

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type sfcAccountMeResponse struct {
	ID string `json:"id"`
}

type sfcZoneListResponse struct {
	Data []sfcZone `json:"data"`
}

type sfcZone struct {
	Name              string                `json:"name"`
	Region            string                `json:"region"`
	HardwareType      string                `json:"hardware_type"`
	InterconnectType  string                `json:"interconnect_type"`
	DeliveryType      string                `json:"delivery_type"`
	AvailableCapacity []sfcZoneAvailability `json:"available_capacity"`
}

type sfcZoneAvailability struct {
	StartTimestamp int64 `json:"start_timestamp"`
	EndTimestamp   int64 `json:"end_timestamp"`
	Quantity       int64 `json:"quantity"`
}

func (c *SFCClient) doJSON(ctx context.Context, method string, path string, body any, out any) error {
	fullURL := strings.TrimRight(c.baseURL, "/") + path

	var requestBody io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		requestBody = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, requestBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("sfc api %s %s failed with status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(rawBody)))
	}

	if out == nil || len(rawBody) == 0 {
		return nil
	}

	if err := json.Unmarshal(rawBody, out); err != nil {
		return fmt.Errorf("decode response body: %w", err)
	}

	return nil
}

func (c *SFCClient) getDefaultWorkspace(ctx context.Context) (string, error) {
	c.workspaceMu.Lock()
	defer c.workspaceMu.Unlock()

	if c.defaultWorkspace != "" {
		return c.defaultWorkspace, nil
	}

	var account sfcAccountMeResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v1/account/me", nil, &account); err != nil {
		return "", err
	}
	if account.ID == "" {
		return "", fmt.Errorf("account response missing id")
	}

	c.defaultWorkspace = fmt.Sprintf("sfc:workspace:%s:default", account.ID)
	return c.defaultWorkspace, nil
}

func (c *SFCClient) getZones(ctx context.Context, includeUnavailable bool) ([]sfcZone, error) {
	var resp sfcZoneListResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v0/zones", nil, &resp); err != nil {
		return nil, err
	}

	zones := make([]sfcZone, 0, len(resp.Data))
	for _, zone := range resp.Data {
		if !hasCurrentCapacity(zone.AvailableCapacity) && !includeUnavailable {
			continue
		}
		if strings.ToUpper(zone.DeliveryType) != deliveryTypeVM {
			continue
		}
		zones = append(zones, zone)
	}

	return zones, nil
}
