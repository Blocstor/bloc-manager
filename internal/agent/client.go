package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// VMInfo holds the libvirt information for a Kubernetes node's underlying VM.
type VMInfo struct {
	Host         string `yaml:"host"`          // KVM host agent name (must match an Agents key)
	Domain       string `yaml:"domain"`        // libvirt domain name
	AttachMethod string `yaml:"attach_method"` // "nbd" (default) or "pcie"
}

// Config holds the agent configuration loaded from agents.yaml.
type Config struct {
	Agents map[string]string `yaml:"agents"`
	VMs    map[string]VMInfo `yaml:"vms"` // Kubernetes node name → VM info
}

// LoadConfig reads and parses the agents YAML configuration file.
func LoadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open agents config %q: %w", path, err)
	}
	defer f.Close()

	var cfg Config
	if err := yaml.NewDecoder(f).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode agents config: %w", err)
	}
	if cfg.Agents == nil {
		cfg.Agents = make(map[string]string)
	}
	return &cfg, nil
}

// Client is an HTTP client for a single bloc-agent instance.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient returns a Client targeting the given base URL.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 120 * time.Second},
	}
}

// errorResponse is the JSON error envelope returned by bloc-agent.
type errorResponse struct {
	Error string `json:"error"`
}

// post sends a POST request with a JSON body and checks for errors.
func (c *Client) post(ctx context.Context, path string, body any) error {
	_, err := c.postResp(ctx, path, body)
	return err
}

// postResp sends a POST request and returns the raw response body on success.
func (c *Client) postResp(ctx context.Context, path string, body any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errResp errorResponse
		if jsonErr := json.Unmarshal(respBody, &errResp); jsonErr == nil && errResp.Error != "" {
			return nil, fmt.Errorf("agent error: %s", errResp.Error)
		}
		return nil, fmt.Errorf("agent returned HTTP %d", resp.StatusCode)
	}

	return respBody, nil
}

// CreateLV creates a logical volume on the agent node.
func (c *Client) CreateLV(ctx context.Context, vg, name string, sizeMB int) error {
	return c.post(ctx, "/lv/create", map[string]any{
		"vg":      vg,
		"name":    name,
		"size_mb": sizeMB,
	})
}

// ExtendLV extends an existing logical volume.
func (c *Client) ExtendLV(ctx context.Context, vg, name string, addMB int) error {
	return c.post(ctx, "/lv/extend", map[string]any{
		"vg":     vg,
		"name":   name,
		"add_mb": addMB,
	})
}

// RemoveLV removes a logical volume.
func (c *Client) RemoveLV(ctx context.Context, vg, name string) error {
	return c.post(ctx, "/lv/remove", map[string]any{
		"vg":   vg,
		"name": name,
	})
}

// WriteRes writes a DRBD .res file on the agent node.
func (c *Client) WriteRes(ctx context.Context, name, content string) error {
	return c.post(ctx, "/res/write", map[string]any{
		"name":    name,
		"content": content,
	})
}

// RemoveRes deletes a DRBD .res file on the agent node.
func (c *Client) RemoveRes(ctx context.Context, name string) error {
	return c.post(ctx, "/res/remove", map[string]any{
		"name": name,
	})
}

// DRBDCreateMD initializes DRBD metadata on a fresh device.
func (c *Client) DRBDCreateMD(ctx context.Context, resource string) error {
	return c.post(ctx, "/drbd/create-md", map[string]any{
		"resource": resource,
	})
}

// DRBDUp brings up a DRBD resource.
func (c *Client) DRBDUp(ctx context.Context, resource string) error {
	return c.post(ctx, "/drbd/up", map[string]any{
		"resource": resource,
	})
}

// DRBDDown brings down a DRBD resource.
func (c *Client) DRBDDown(ctx context.Context, resource string) error {
	return c.post(ctx, "/drbd/down", map[string]any{
		"resource": resource,
	})
}

// DRBDPrimary promotes a DRBD resource to Primary.
func (c *Client) DRBDPrimary(ctx context.Context, resource string) error {
	return c.post(ctx, "/drbd/primary", map[string]any{
		"resource": resource,
	})
}

// DRBDPrimaryForce forcibly promotes a DRBD resource to Primary.
// Use when both peers are Inconsistent (fresh device initial setup).
func (c *Client) DRBDPrimaryForce(ctx context.Context, resource string) error {
	return c.post(ctx, "/drbd/primary-force", map[string]any{
		"resource": resource,
	})
}

// DRBDSecondary demotes a DRBD resource to Secondary.
func (c *Client) DRBDSecondary(ctx context.Context, resource string) error {
	return c.post(ctx, "/drbd/secondary", map[string]any{
		"resource": resource,
	})
}

// DRBDResize triggers a drbdadm resize on the resource.
func (c *Client) DRBDResize(ctx context.Context, resource string) error {
	return c.post(ctx, "/drbd/resize", map[string]any{
		"resource": resource,
	})
}

// DRBDStatus returns the status string for a DRBD resource.
func (c *Client) DRBDStatus(ctx context.Context, resource string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/drbd/status?resource="+resource, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e errorResponse
		if jsonErr := json.Unmarshal(body, &e); jsonErr == nil && e.Error != "" {
			return "", fmt.Errorf("agent error: %s", e.Error)
		}
		return "", fmt.Errorf("agent returned HTTP %d", resp.StatusCode)
	}
	var result struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode status response: %w", err)
	}
	return result.Output, nil
}

// ClusterQuorate returns true if the KVM host's Pacemaker/Corosync cluster has quorum.
func (c *Client) ClusterQuorate(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/quorum", nil)
	if err != nil {
		return false, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e errorResponse
		if jsonErr := json.Unmarshal(body, &e); jsonErr == nil && e.Error != "" {
			return false, fmt.Errorf("agent error: %s", e.Error)
		}
		return false, fmt.Errorf("agent returned HTTP %d", resp.StatusCode)
	}
	var result struct {
		Quorate bool `json:"quorate"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return false, fmt.Errorf("decode quorum response: %w", err)
	}
	return result.Quorate, nil
}

// VMBlockList returns the virtio block device targets attached to a VM domain.
func (c *Client) VMBlockList(ctx context.Context, domain string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/vm/blklist?domain="+domain, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e errorResponse
		if jsonErr := json.Unmarshal(body, &e); jsonErr == nil && e.Error != "" {
			return nil, fmt.Errorf("agent error: %s", e.Error)
		}
		return nil, fmt.Errorf("agent returned HTTP %d", resp.StatusCode)
	}
	var result struct {
		Targets []string `json:"targets"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return result.Targets, nil
}

// VMAttach hot-attaches a block device to a running libvirt domain.
func (c *Client) VMAttach(ctx context.Context, domain, source, target string) error {
	return c.post(ctx, "/vm/attach", map[string]any{
		"domain": domain,
		"source": source,
		"target": target,
	})
}

// VMDetach hot-detaches a virtio disk from a running libvirt domain by target name.
func (c *Client) VMDetach(ctx context.Context, domain, target string) error {
	return c.post(ctx, "/vm/detach", map[string]any{
		"domain": domain,
		"target": target,
	})
}

// NBDServe starts a qemu-nbd server on the agent node exposing device on port.
func (c *Client) NBDServe(ctx context.Context, device string, port int) error {
	return c.post(ctx, "/nbd/serve", map[string]any{
		"device": device,
		"port":   port,
	})
}

// NBDStop kills the qemu-nbd server running on the given port.
func (c *Client) NBDStop(ctx context.Context, port int) error {
	return c.post(ctx, "/nbd/stop", map[string]any{
		"port": port,
	})
}
