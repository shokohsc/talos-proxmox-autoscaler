package proxmox

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

type AuthType int

const (
	AuthPassword AuthType = iota
	AuthToken
)

type Client struct {
	httpClient  *http.Client
	baseURL     string
	tokenID     string
	tokenSecret string
	node        string // configured default node; may be empty
	// resolved is the last known valid default node, refreshed by ResolveNode.
	// Kept separate from node so a configured node that temporarily vanishes
	// cannot permanently repoint default placement.
	resolved  string
	authType  AuthType
	ticket    string
	csrfToken string
}

type PCIDevice struct {
	ID   string `json:"id"`
	PCIe bool   `json:"pcie"`
	GPU  bool   `json:"gpu"`
	// MediatedDevice specifies the mdev type for mediated device passthrough
	// (e.g., "nvidia-11", "i915-GVTg_V5_4", or a UUID for pre-created mdevs).
	MediatedDevice string `json:"mediated_device,omitempty"`
	// Mdev is an alias for MediatedDevice for compatibility.
	Mdev string `json:"mdev,omitempty"`
	// Additional fields for flexibility
	Mediated   string `json:"mediated,omitempty"`
	PCIAddress string `json:"pci_address,omitempty"`
	Address    string `json:"address,omitempty"`
	DeviceID   string `json:"device_id,omitempty"`
}

type VMConfig struct {
	Name          string
	Node          string // target Proxmox node; empty means use the client's default
	VMID          int
	VCPU          int32
	MemoryMiB     int32
	DiskGiB       int32
	StoragePool   string
	NetworkBridge string
	MACAddress    string
	Serial        string
	CPUType       string
	TemplateID    int
	Tags          string
	VLANID        int
	PCIDevices    []PCIDevice
}

type Node struct {
	Node      string  `json:"node"`
	Status    string  `json:"status"`
	CPU       float64 `json:"cpu"`
	MaxCPU    int     `json:"maxcpu"`
	Memory    int     `json:"mem"`
	MaxMemory int     `json:"maxmem"`
}

type apiResponse struct {
	Data json.RawMessage `json:"data"`
}

func NewClient(baseURL, username, password, tokenID, tokenSecret, node string, insecure bool) (*Client, error) {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure},
	}
	c := &Client{
		httpClient: &http.Client{Transport: transport, Timeout: 30 * time.Second},
		baseURL:    strings.TrimSuffix(baseURL, "/"),
		node:       node,
	}

	if password != "" && username != "" {
		c.authType = AuthPassword
		c.tokenID = username
		c.tokenSecret = password
	} else if tokenID != "" && tokenSecret != "" {
		c.authType = AuthToken
		c.tokenID = tokenID
		c.tokenSecret = tokenSecret
	} else {
		return nil, fmt.Errorf("no valid auth credentials provided")
	}

	return c, nil
}

func (c *Client) login() error {
	if c.authType == AuthToken {
		return nil
	}

	loginURL := c.baseURL + "/api2/json/access/ticket"
	data := "username=" + url.QueryEscape(c.tokenID) + "&password=" + url.QueryEscape(c.tokenSecret)

	resp, err := c.httpClient.Post(loginURL, "application/x-www-form-urlencoded", strings.NewReader(data))
	if err != nil {
		return fmt.Errorf("login request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("login failed: %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Data struct {
			Ticket              string `json:"ticket"`
			CSRFPreventionToken string `json:"CSRFPreventionToken"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode login response: %w", err)
	}
	if result.Data.Ticket == "" {
		return fmt.Errorf("login returned empty ticket")
	}

	c.ticket = result.Data.Ticket
	c.csrfToken = result.Data.CSRFPreventionToken
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body interface{}) (json.RawMessage, error) {
	var payload []byte
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		payload = data
	}

	// buildReq is called again on auth retry: a request body reader is
	// consumed by the first attempt, so the retry needs a fresh request.
	buildReq := func() (*http.Request, error) {
		var reqBody io.Reader
		if payload != nil {
			reqBody = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		if c.authType == AuthToken {
			req.Header.Set("Authorization", "PVEAPIToken="+c.tokenID+"="+c.tokenSecret)
		} else {
			if c.ticket == "" {
				if err := c.login(); err != nil {
					return nil, err
				}
			}
			req.Header.Set("Cookie", "PVEAuthCookie="+c.ticket)
			req.Header.Set("CSRFPreventionToken", c.csrfToken)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		return req, nil
	}

	send := func(req *http.Request) ([]byte, int, error) {
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, 0, fmt.Errorf("request failed: %w", err)
		}
		respBody, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
		}
		zap.S().Debugw("API call", "method", method, "url", req.URL.RequestURI(), "status", resp.StatusCode)
		return respBody, resp.StatusCode, nil
	}

	req, err := buildReq()
	if err != nil {
		return nil, err
	}
	respBody, status, err := send(req)
	if err != nil {
		return nil, err
	}

	// A 401 on a cached password-auth ticket means the ticket expired or was
	// invalidated (Proxmox allows one ticket per user/IP; another login kills it).
	// Re-login once and retry instead of failing forever on a stale cookie.
	if status == http.StatusUnauthorized && c.authType == AuthPassword {
		c.ticket = ""
		if err := c.login(); err != nil {
			return nil, err
		}
		req, err = buildReq()
		if err != nil {
			return nil, err
		}
		respBody, status, err = send(req)
		if err != nil {
			return nil, err
		}
	}

	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("API error %d: %s", status, respBody)
	}

	var apiResp apiResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return apiResp.Data, nil
}

func (c *Client) CreateVM(ctx context.Context, config VMConfig) (string, error) {
	node := c.nodeFor(config.Node)
	zap.S().Infow("Creating VM", "vmid", config.VMID, "name", config.Name, "proxmox_node", node, "cpu", config.VCPU, "memory_mib", config.MemoryMiB)

	if config.TemplateID > 0 {
		if err := c.cloneVM(ctx, config); err != nil {
			return "", err
		}
	} else {
		if err := c.createVMFromScratch(ctx, config); err != nil {
			return "", err
		}
	}

	if err := c.startVM(ctx, node, config.VMID); err != nil {
		return "", fmt.Errorf("start VM %d: %w", config.VMID, err)
	}

	ip, err := c.waitForIP(ctx, node, config.VMID, 5*time.Minute)
	if err != nil {
		return "", fmt.Errorf("wait for IP of VM %d: %w", config.VMID, err)
	}

	zap.S().Infow("VM created and started", "vmid", config.VMID, "name", config.Name, "ip", ip)
	return ip, nil
}

func (c *Client) createVMFromScratch(ctx context.Context, config VMConfig) error {
	mac := config.MACAddress
	if mac == "" {
		mac = randomMAC()
	}

	params := url.Values{}
	params.Set("vmid", strconv.Itoa(config.VMID))
	params.Set("name", config.Name)
	params.Set("cores", strconv.Itoa(int(config.VCPU)))
	params.Set("memory", strconv.Itoa(int(config.MemoryMiB)))
	params.Set("scsihw", "virtio-scsi-single")
	params.Set("scsi0", fmt.Sprintf("%s:0,iothread=1", config.StoragePool))
	params.Set("net0", fmt.Sprintf("virtio=%s,bridge=%s", mac, config.NetworkBridge))
	params.Set("boot", "order=scsi0;net0")
	params.Set("agent", "1")

	node := c.nodeFor(config.Node)

	cpuType := config.CPUType
	if cpuType == "" {
		cpuType = "host"
	}
	params.Set("cpu", cpuType)

	if config.VLANID > 0 {
		params.Set("net0", fmt.Sprintf("virtio=%s,bridge=%s,tag=%d", mac, config.NetworkBridge, config.VLANID))
	}

	if config.DiskGiB > 0 {
		params.Set("scsi0", fmt.Sprintf("%s:%d,iothread=1", config.StoragePool, config.DiskGiB))
	}
	if config.Serial != "" {
		params.Set("smbios1", fmt.Sprintf("serial=%s,uuid=%s,base64=1", base64.StdEncoding.EncodeToString([]byte(config.Serial)), randomUUID()))
	}
	if config.Tags != "" {
		params.Set("tags", config.Tags)
	}
	for i, pci := range config.PCIDevices {
		key := fmt.Sprintf("hostpci%d", i)
		hostID := pci.ID
		if hostID == "" {
			hostID = pci.PCIAddress
		}
		if hostID == "" {
			hostID = pci.Address
		}
		if hostID == "" {
			hostID = pci.DeviceID
		}
		value := fmt.Sprintf("host=%s", hostID)
		if pci.PCIe {
			value += ",pcie=1"
		}
		if pci.GPU {
			value += ",gpu=1"
		}
		mdev := pci.MediatedDevice
		if mdev == "" {
			mdev = pci.Mdev
		}
		if mdev == "" {
			mdev = pci.Mediated
		}
		if mdev != "" {
			value += fmt.Sprintf(",mdev=%s", mdev)
		}
		params.Set(key, value)
	}

	_, err := c.do(ctx, "POST", fmt.Sprintf("/api2/json/nodes/%s/qemu?%s", node, params.Encode()), nil)
	return err
}

func (c *Client) cloneVM(ctx context.Context, config VMConfig) error {
	target := c.nodeFor(config.Node)
	// The clone task runs on the node hosting the template; the new VM lands
	// on target, which may be a different node.
	source := c.vmNode(ctx, config.TemplateID)
	body := map[string]interface{}{
		"newid":  config.VMID,
		"name":   config.Name,
		"target": target,
	}

	data, err := c.do(ctx, "POST", fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/clone", source, config.TemplateID), body)
	if err != nil {
		return fmt.Errorf("clone VM %d: %w", config.TemplateID, err)
	}

	// Wait for clone to finish
	var taskID string
	_ = json.Unmarshal(data, &taskID)
	if taskID != "" {
		if err := c.waitForTask(ctx, source, taskID); err != nil {
			return fmt.Errorf("wait for clone task: %w", err)
		}
	}

	// Resize disk and set config
	params := url.Values{}
	params.Set("cores", strconv.Itoa(int(config.VCPU)))
	params.Set("memory", strconv.Itoa(int(config.MemoryMiB)))
	if config.DiskGiB > 0 {
		params.Set("scsi0", fmt.Sprintf("%s:%d,iothread=1", config.StoragePool, config.DiskGiB))
	}
	if config.MACAddress != "" {
		net0 := fmt.Sprintf("virtio=%s,bridge=%s", config.MACAddress, config.NetworkBridge)
		if config.VLANID > 0 {
			net0 += fmt.Sprintf(",tag=%d", config.VLANID)
		}
		params.Set("net0", net0)
	}
	if config.Serial != "" {
		params.Set("smbios1", fmt.Sprintf("serial=%s,uuid=%s,base64=1", base64.StdEncoding.EncodeToString([]byte(config.Serial)), randomUUID()))
	}
	if config.Tags != "" {
		params.Set("tags", config.Tags)
	}
	for i, pci := range config.PCIDevices {
		key := fmt.Sprintf("hostpci%d", i)
		hostID := pci.ID
		if hostID == "" {
			hostID = pci.PCIAddress
		}
		if hostID == "" {
			hostID = pci.Address
		}
		if hostID == "" {
			hostID = pci.DeviceID
		}
		value := fmt.Sprintf("host=%s", hostID)
		if pci.PCIe {
			value += ",pcie=1"
		}
		if pci.GPU {
			value += ",gpu=1"
		}
		mdev := pci.MediatedDevice
		if mdev == "" {
			mdev = pci.Mdev
		}
		if mdev == "" {
			mdev = pci.Mediated
		}
		if mdev != "" {
			value += fmt.Sprintf(",mdev=%s", mdev)
		}
		params.Set(key, value)
	}

	if _, err := c.do(ctx, "PUT", fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/config?%s", target, config.VMID, params.Encode()), nil); err != nil {
		return fmt.Errorf("configure cloned VM: %w", err)
	}

	return nil
}

func (c *Client) startVM(ctx context.Context, node string, vmid int) error {
	_, err := c.do(ctx, "POST", fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/status/start", node, vmid), nil)
	return err
}

// vmNode resolves the Proxmox node hosting a VM. Cluster resources is the only
// placement-independent source; with VMs spread across nodes, c.node cannot be
// assumed. Falls back to c.node when the lookup fails or the VM is unknown.
func (c *Client) vmNode(ctx context.Context, vmid int) string {
	if vms, err := c.ListVMs(ctx); err == nil {
		for _, vm := range vms {
			if vm.VMID == vmid {
				return vm.Node
			}
		}
	}
	return c.nodeFor("")
}

// nodeFor returns the per-VM target node, falling back to the client default.
func (c *Client) nodeFor(configNode string) string {
	if configNode != "" {
		return configNode
	}
	if c.node != "" {
		return c.node
	}
	return c.resolved
}

func (c *Client) stopVMOn(ctx context.Context, node string, vmid int) error {
	_, err := c.do(ctx, "POST", fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/status/stop", node, vmid), nil)
	return err
}

func (c *Client) StopVM(ctx context.Context, vmid int) error {
	return c.stopVMOn(ctx, c.vmNode(ctx, vmid), vmid)
}

// vmShutdownWait is how long DeleteVMOn waits for a graceful shutdown before
// falling back to a hard stop. vmHardStopWait is how long it waits for the
// hard stop to take effect. Package vars so tests can shrink them.
var (
	vmShutdownWait = 30 * time.Second
	vmHardStopWait = 10 * time.Second
)

// DeleteVMOn deletes a VM on the given Proxmox node. An empty node triggers a
// cluster-wide lookup of the node hosting the VM. It shuts the VM down
// gracefully first (qemu-guest-agent based) and only hard stops it if the
// shutdown does not complete in time.
func (c *Client) DeleteVMOn(ctx context.Context, node string, vmid int) error {
	if node == "" {
		node = c.vmNode(ctx, vmid)
	}

	// Graceful shutdown first, so the guest OS (Talos) flushes state.
	_, _ = c.do(ctx, "POST", fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/status/shutdown", node, vmid), nil)
	if !c.waitForStopped(ctx, node, vmid, vmShutdownWait) {
		zap.S().Warnw("graceful shutdown timed out, hard stopping VM", "vmid", vmid, "proxmox_node", node)
		_ = c.stopVMOn(ctx, node, vmid)
		c.waitForStopped(ctx, node, vmid, vmHardStopWait)
	}

	_, err := c.do(ctx, "DELETE", fmt.Sprintf("/api2/json/nodes/%s/qemu/%d", node, vmid), nil)
	if err != nil {
		return fmt.Errorf("delete VM %d: %w", vmid, err)
	}

	zap.S().Infow("VM deleted", "vmid", vmid, "proxmox_node", node)
	return nil
}

// DeleteVM deletes a VM wherever it lives in the cluster.
func (c *Client) DeleteVM(ctx context.Context, vmid int) error {
	return c.DeleteVMOn(ctx, "", vmid)
}

// waitForStopped polls the VM status until it reports "stopped", the wait
// elapses, or the context is cancelled. Returns true only when stopped.
func (c *Client) waitForStopped(ctx context.Context, node string, vmid int, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		data, err := c.do(ctx, "GET", fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/status/current", node, vmid), nil)
		if err == nil {
			var status struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(data, &status) == nil && status.Status == "stopped" {
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
	return false
}

func (c *Client) waitForIP(ctx context.Context, node string, vmid int, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := c.do(ctx, "GET", fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/agent/network-get-interfaces", node, vmid), nil)
		if err == nil {
			var ifaces []struct {
				Name        string `json:"name"`
				IPAddresses []struct {
					IPAddress string `json:"ip-address"`
					IPType    string `json:"ip-address-type"`
				} `json:"ip-addresses"`
			}
			if json.Unmarshal(data, &ifaces) == nil {
				for _, iface := range ifaces {
					if iface.Name == "lo" {
						continue
					}
					for _, addr := range iface.IPAddresses {
						if addr.IPType == "ipv4" && addr.IPAddress != "" {
							return addr.IPAddress, nil
						}
					}
				}
			}
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return "", fmt.Errorf("timeout waiting for IP of VM %d", vmid)
}

func (c *Client) waitForTask(ctx context.Context, node string, upid string) error {
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		data, err := c.do(ctx, "GET", fmt.Sprintf("/api2/json/nodes/%s/tasks/%s", node, url.PathEscape(upid)), nil)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		var task struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(data, &task) == nil && task.Status == "stopped" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("timeout waiting for task %s", upid)
}

// VM is a virtual machine or container visible cluster-wide (qemu + lxc).
type VM struct {
	VMID     int    `json:"vmid"`
	Name     string `json:"name"`
	Node     string `json:"node"`
	Status   string `json:"status"`
	Type     string `json:"type"`
	Template int    `json:"template"`
	Tags     string `json:"tags"` // "," or ";" separated string
	// Uptime is seconds since the VM last started. Cluster resources only
	// reports it for running guests, so it is 0 for a VM that never started.
	Uptime int64 `json:"uptime"`
}

// ListVMs returns every VM in the cluster. The API token needs the Audit
// privilege on the /vm/ ACL path for tags to be visible.
func (c *Client) ListVMs(ctx context.Context) ([]VM, error) {
	data, err := c.do(ctx, "GET", "/api2/json/cluster/resources?type=vm", nil)
	if err != nil {
		return nil, err
	}
	var vms []VM
	if err := json.Unmarshal(data, &vms); err != nil {
		return nil, err
	}
	return vms, nil
}

func (c *Client) ListNodes(ctx context.Context) ([]string, error) {
	data, err := c.do(ctx, "GET", "/api2/json/nodes", nil)
	if err != nil {
		return nil, err
	}

	var nodes []Node
	if err := json.Unmarshal(data, &nodes); err != nil {
		return nil, err
	}

	var online []string
	for _, n := range nodes {
		if n.Status == "online" {
			online = append(online, n.Node)
		}
	}
	sort.Strings(online)
	return online, nil
}

// SetVMConfigOn updates QEMU configuration on the given Proxmox node. An
// empty node triggers a cluster-wide lookup of the node hosting the VM.
func (c *Client) SetVMConfigOn(ctx context.Context, node string, vmid int, params url.Values) error {
	if node == "" {
		node = c.vmNode(ctx, vmid)
	}
	if params == nil {
		params = url.Values{}
	}
	_, err := c.do(ctx, "PUT", fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/config?%s", node, vmid, params.Encode()), nil)
	return err
}

func (c *Client) GetNode(ctx context.Context) (string, error) {
	nodes, err := c.ListNodes(ctx)
	if err != nil {
		return "", err
	}
	if len(nodes) == 0 {
		return "", fmt.Errorf("no active nodes found")
	}

	if c.node != "" {
		for _, n := range nodes {
			if n == c.node {
				return c.node, nil
			}
		}
		zap.S().Warnf("Configured node %q not found in cluster, using first available: %s", c.node, nodes[0])
	}

	return nodes[0], nil
}

// ResolveNode caches a valid default cluster node for placement fallback.
// The configured node itself is never overwritten.
func (c *Client) ResolveNode(ctx context.Context) error {
	node, err := c.GetNode(ctx)
	if err != nil {
		return err
	}
	c.resolved = node
	return nil
}

func randomMAC() string {
	b := make([]byte, 4)
	_, _ = crand.Read(b)
	return fmt.Sprintf("52:54:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3])
}

func randomUUID() string {
	var uuid [16]byte
	_, _ = crand.Read(uuid[:])
	uuid[6] = (uuid[6] & 0x0f) | 0x40 // version 4
	uuid[8] = (uuid[8] & 0x3f) | 0x80 // variant RFC 4122
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:16])
}
