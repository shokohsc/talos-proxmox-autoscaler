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

	"k8s.io/klog/v2"
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
	node        string
	authType    AuthType
	ticket      string
	csrfToken   string
}

type PCIDevice struct {
	ID   string `json:"id"`
	PCIe bool   `json:"pcie"`
	GPU  bool   `json:"gpu"`
}

type VMConfig struct {
	Name          string
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
	PCIDevices   []PCIDevice
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
	defer resp.Body.Close()

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
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	u := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
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
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	klog.V(2).Info("API call", "method", method, "url", req.URL.RequestURI(), "status", resp.StatusCode)

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp apiResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return apiResp.Data, nil
}

func (c *Client) CreateVM(ctx context.Context, config VMConfig) (string, error) {
	log := klog.FromContext(ctx)
	klog.V(1).Info("Creating VM", "vmid", config.VMID, "name", config.Name, "cpu", config.VCPU, "memory_mib", config.MemoryMiB)

	if config.TemplateID > 0 {
		if err := c.cloneVM(ctx, config); err != nil {
			return "", err
		}
	} else {
		if err := c.createVMFromScratch(ctx, config); err != nil {
			return "", err
		}
	}

	if err := c.startVM(ctx, config.VMID); err != nil {
		return "", fmt.Errorf("start VM %d: %w", config.VMID, err)
	}

	ip, err := c.waitForIP(ctx, config.VMID, 5*time.Minute)
	if err != nil {
		return "", fmt.Errorf("wait for IP of VM %d: %w", config.VMID, err)
	}

	log.Info("VM created and started", "vmid", config.VMID, "name", config.Name, "ip", ip)
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
		value := fmt.Sprintf("host=%s", pci.ID)
		if pci.PCIe {
			value += ",pcie=1"
		}
		if pci.GPU {
			value += ",gpu=1"
		}
		params.Set(key, value)
	}

	_, err := c.do(ctx, "POST", fmt.Sprintf("/api2/json/nodes/%s/qemu?%s", c.node, params.Encode()), nil)
	return err
}

func (c *Client) cloneVM(ctx context.Context, config VMConfig) error {
	body := map[string]interface{}{
		"newid":  config.VMID,
		"name":   config.Name,
		"target": c.node,
	}

	data, err := c.do(ctx, "POST", fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/clone", c.node, config.TemplateID), body)
	if err != nil {
		return fmt.Errorf("clone VM %d: %w", config.TemplateID, err)
	}

	// Wait for clone to finish
	var taskID string
	_ = json.Unmarshal(data, &taskID)
	if taskID != "" {
		if err := c.waitForTask(ctx, taskID); err != nil {
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

	if _, err := c.do(ctx, "PUT", fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/config?%s", c.node, config.VMID, params.Encode()), nil); err != nil {
		return fmt.Errorf("configure cloned VM: %w", err)
	}

	return nil
}

func (c *Client) startVM(ctx context.Context, vmid int) error {
	_, err := c.do(ctx, "POST", fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/status/start", c.node, vmid), nil)
	return err
}

func (c *Client) StopVM(ctx context.Context, vmid int) error {
	_, err := c.do(ctx, "POST", fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/status/stop", c.node, vmid), nil)
	return err
}

func (c *Client) DeleteVM(ctx context.Context, vmid int) error {
	log := klog.FromContext(ctx)

	// Try to stop first (ignore error if already stopped)
	_ = c.StopVM(ctx, vmid)

	// Wait a bit for shutdown
	time.Sleep(3 * time.Second)

	_, err := c.do(ctx, "DELETE", fmt.Sprintf("/api2/json/nodes/%s/qemu/%d", c.node, vmid), nil)
	if err != nil {
		return fmt.Errorf("delete VM %d: %w", vmid, err)
	}

	log.Info("VM deleted", "vmid", vmid)
	return nil
}

func (c *Client) GetVMStatus(ctx context.Context, vmid int) (string, error) {
	data, err := c.do(ctx, "GET", fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/status/current", c.node, vmid), nil)
	if err != nil {
		return "", err
	}
	var status struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return "", err
	}
	return status.Status, nil
}

func (c *Client) FindVMByName(ctx context.Context, name string) (int, error) {
	data, err := c.do(ctx, "GET", fmt.Sprintf("/api2/json/nodes/%s/qemu?filter=name=%s", c.node, url.QueryEscape(name)), nil)
	if err != nil {
		return 0, err
	}

	var vms []struct {
		VMID   int    `json:"vmid"`
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &vms); err != nil {
		return 0, err
	}
	for _, vm := range vms {
		if vm.Name == name {
			return vm.VMID, nil
		}
	}
	return 0, fmt.Errorf("VM %q not found", name)
}

func (c *Client) waitForIP(ctx context.Context, vmid int, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := c.do(ctx, "GET", fmt.Sprintf("/api2/json/nodes/%s/qemu/%d/agent/network-get-interfaces", c.node, vmid), nil)
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

func (c *Client) waitForTask(ctx context.Context, upid string) error {
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		data, err := c.do(ctx, "GET", fmt.Sprintf("/api2/json/nodes/%s/tasks/%s", c.node, url.PathEscape(upid)), nil)
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
		klog.Warningf("Configured node %q not found in cluster, using first available: %s", c.node, nodes[0])
	}

	return nodes[0], nil
}

// ResolveNode sets the client's node to a valid cluster node.
// Must be called before API operations that target a specific node.
func (c *Client) ResolveNode(ctx context.Context) error {
	node, err := c.GetNode(ctx)
	if err != nil {
		return err
	}
	c.node = node
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
