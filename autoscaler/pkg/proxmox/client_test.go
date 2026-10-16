package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthDetection_PasswordAuth(t *testing.T) {
	client, err := NewClient(
		"https://pve.example.com:8006",
		"root@pam",
		"password123",
		"",
		"",
		"pve",
		true,
	)
	assert.NoError(t, err)
	assert.Equal(t, AuthPassword, client.authType)
}

func TestAuthDetection_TokenAuth(t *testing.T) {
	client, err := NewClient(
		"https://pve.example.com:8006",
		"",
		"",
		"user@realm!tokenid",
		"uuid-here",
		"pve",
		true,
	)
	assert.NoError(t, err)
	assert.Equal(t, AuthToken, client.authType)
}

func TestAuthDetection_NoCredentials(t *testing.T) {
	_, err := NewClient(
		"https://pve.example.com:8006",
		"",
		"",
		"",
		"",
		"pve",
		true,
	)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no valid auth credentials")
}

func TestLogin_Success(t *testing.T) {
	var gotUser, gotPass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api2/json/access/ticket" {
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			gotUser = r.FormValue("username")
			gotPass = r.FormValue("password")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]string{
					"ticket":              "PVEticket123",
					"CSRFPreventionToken": "csrf456",
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "root@pam", "secret", "", "", "pve", true)
	assert.NoError(t, err)
	assert.NoError(t, c.login())

	assert.Equal(t, "root@pam", gotUser)
	assert.Equal(t, "secret", gotPass)
	assert.Equal(t, "PVEticket123", c.ticket)
	assert.Equal(t, "csrf456", c.csrfToken)
}

func TestLogin_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":{"username":"invalid"}}`))
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "root@pam", "wrong", "", "", "pve", true)
	assert.NoError(t, err)
	assert.Error(t, c.login())
}

func TestLogin_TokenAuthSkipsLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called for token auth")
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	assert.NoError(t, err)
	assert.NoError(t, c.login())
	assert.Equal(t, "", c.ticket)
}

func TestDo_TriggersLoginOnPasswordAuth(t *testing.T) {
	var ticketRequested bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api2/json/access/ticket" {
			ticketRequested = true
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]string{
					"ticket":              "PVEticket",
					"CSRFPreventionToken": "csrf",
				},
			})
			return
		}
		cookie := r.Header.Get("Cookie")
		csrf := r.Header.Get("CSRFPreventionToken")
		if cookie != "PVEAuthCookie=PVEticket" || csrf != "csrf" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"data":null}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": json.RawMessage(`{"version":"8.1.4"}`),
		})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "root@pam", "pass", "", "", "pve", true)
	assert.NoError(t, err)
	assert.False(t, ticketRequested)
	assert.Equal(t, "", c.ticket)

	_, err = c.do(context.Background(), "GET", "/api2/json/version", nil)
	assert.NoError(t, err)
	assert.True(t, ticketRequested)
	assert.Equal(t, "PVEticket", c.ticket)
}

func TestListNodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []Node{
				{Node: "pve2", Status: "online"},
				{Node: "pve1", Status: "online"},
				{Node: "pve3", Status: "offline"},
			},
		})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "", true)
	assert.NoError(t, err)

	nodes, err := c.ListNodes(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, []string{"pve1", "pve2"}, nodes)
}

func TestListNodes_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []Node{},
		})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "", true)
	assert.NoError(t, err)

	nodes, err := c.ListNodes(context.Background())
	assert.NoError(t, err)
	assert.Empty(t, nodes)
}

func TestGetNode_Configured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []Node{
				{Node: "pve1", Status: "online"},
				{Node: "pve2", Status: "online"},
			},
		})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve1", true)
	assert.NoError(t, err)

	node, err := c.GetNode(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, "pve1", node)
}

func TestGetNode_ConfiguredNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []Node{
				{Node: "node-a", Status: "online"},
				{Node: "node-b", Status: "online"},
			},
		})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	assert.NoError(t, err)

	node, err := c.GetNode(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, "node-a", node) // falls back to first available
}

func TestGetNode_AutoDiscover(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []Node{
				{Node: "pve2", Status: "online"},
				{Node: "pve1", Status: "online"},
			},
		})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "", true)
	assert.NoError(t, err)

	node, err := c.GetNode(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, "pve1", node) // first after sort
}

func TestGetNode_NoNodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []Node{},
		})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "", true)
	assert.NoError(t, err)

	_, err = c.GetNode(context.Background())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no active nodes")
}

func TestResolveNode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api2/json/nodes" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []Node{
					{Node: "pve1", Status: "online"},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "", true)
	require.NoError(t, err)

	err = c.ResolveNode(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, "pve1", c.resolved)
}

func TestPasswordAuth_ReloginOn401(t *testing.T) {
	loginCount := 0
	apiCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/access/ticket":
			loginCount++
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]string{
					"ticket":              "PVEticket",
					"CSRFPreventionToken": "csrf",
				},
			})
		default:
			apiCount++
			// First API call uses the cached (stale) ticket -> reject.
			if apiCount == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"data":null}`))
				return
			}
			assert.Equal(t, "PVEAuthCookie=PVEticket", r.Header.Get("Cookie"))
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": json.RawMessage(`"ok"`),
			})
		}
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "root@pam", "pass", "", "", "pve", true)
	require.NoError(t, err)
	c.ticket = "PVEexpired" // simulate a ticket that has since expired
	c.csrfToken = "csrf"

	_, err = c.do(context.Background(), "GET", "/api2/json/version", nil)
	assert.NoError(t, err)
	assert.Equal(t, 1, loginCount) // only the re-login after the stale-ticket 401
}

func TestPasswordAuth_CachesTicket(t *testing.T) {
	loginCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api2/json/access/ticket" {
			loginCount++
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]string{
					"ticket":              "PVEticket",
					"CSRFPreventionToken": "csrf",
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": json.RawMessage(`"ok"`),
		})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "root@pam", "pass", "", "", "pve", true)
	assert.NoError(t, err)

	_, err = c.do(context.Background(), "GET", "/api2/json/version", nil)
	assert.NoError(t, err)
	assert.Equal(t, 1, loginCount)

	_, err = c.do(context.Background(), "GET", "/api2/json/version", nil)
	assert.NoError(t, err)
	assert.Equal(t, 1, loginCount)
}

func TestCreateVMFromScratch_Tags(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	assert.NoError(t, err)

	err = c.createVMFromScratch(context.Background(), VMConfig{
		Name:        "test-vm",
		VMID:        100,
		VCPU:        2,
		MemoryMiB:   2048,
		StoragePool: "local-lvm",
		Tags:        "autoscaler,worker,v1",
	})
	assert.NoError(t, err)
	assert.Contains(t, gotQuery, "tags=autoscaler%2Cworker%2Cv1")
}

func TestCreateVMFromScratch_NoTagsWhenEmpty(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	assert.NoError(t, err)

	err = c.createVMFromScratch(context.Background(), VMConfig{
		Name:        "test-vm",
		VMID:        100,
		VCPU:        2,
		MemoryMiB:   2048,
		StoragePool: "local-lvm",
	})
	assert.NoError(t, err)
	assert.NotContains(t, gotQuery, "tags")
}

func TestCreateVMFromScratch_PCI(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	assert.NoError(t, err)

	err = c.createVMFromScratch(context.Background(), VMConfig{
		Name:        "test-vm",
		VMID:        100,
		VCPU:        2,
		MemoryMiB:   2048,
		StoragePool: "local-lvm",
		PCIDevices: []PCIDevice{
			{ID: "0000:01:00.0", PCIe: true, GPU: true},
		},
	})
	assert.NoError(t, err)
	assert.Contains(t, gotQuery, "hostpci0")
	assert.Contains(t, gotQuery, "host%3D0000%3A01%3A00.0")
	assert.Contains(t, gotQuery, "pcie%3D1")
	assert.Contains(t, gotQuery, "gpu%3D1")
}

func TestCreateVMFromScratch_MultiplePCI(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	assert.NoError(t, err)

	err = c.createVMFromScratch(context.Background(), VMConfig{
		Name:        "test-vm",
		VMID:        100,
		VCPU:        2,
		MemoryMiB:   2048,
		StoragePool: "local-lvm",
		PCIDevices: []PCIDevice{
			{ID: "0000:01:00.0", PCIe: true, GPU: false},
			{ID: "0000:02:00.0", PCIe: false, GPU: true},
		},
	})
	assert.NoError(t, err)
	assert.Contains(t, gotQuery, "hostpci0")
	assert.Contains(t, gotQuery, "hostpci1")
}

func TestCreateVMFromScratch_VLANID(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	assert.NoError(t, err)

	err = c.createVMFromScratch(context.Background(), VMConfig{
		Name:          "test-vm",
		VMID:          100,
		VCPU:          2,
		MemoryMiB:     2048,
		StoragePool:   "local-lvm",
		NetworkBridge: "vmbr0",
		VLANID:        100,
	})
	assert.NoError(t, err)
	assert.Contains(t, gotQuery, "net0")
	assert.Contains(t, gotQuery, "tag%3D100")
}

func TestCreateVMFromScratch_NoVLANWhenZero(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	assert.NoError(t, err)

	err = c.createVMFromScratch(context.Background(), VMConfig{
		Name:          "test-vm",
		VMID:          100,
		VCPU:          2,
		MemoryMiB:     2048,
		StoragePool:   "local-lvm",
		NetworkBridge: "vmbr0",
		VLANID:        0,
	})
	assert.NoError(t, err)
	assert.Contains(t, gotQuery, "net0")
	assert.NotContains(t, gotQuery, "tag")
}

func TestCreateVMFromScratch_AutoMAC(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	assert.NoError(t, err)

	err = c.createVMFromScratch(context.Background(), VMConfig{
		Name:          "test-vm",
		VMID:          100,
		VCPU:          2,
		MemoryMiB:     2048,
		StoragePool:   "local-lvm",
		NetworkBridge: "vmbr0",
	})
	assert.NoError(t, err)
	assert.Contains(t, gotQuery, "net0")
	assert.Contains(t, gotQuery, "52%3A54") // URL-encoded "52:54" prefix
}

func TestCreateVMFromScratch_Serial(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	assert.NoError(t, err)

	err = c.createVMFromScratch(context.Background(), VMConfig{
		Name:          "test-vm",
		VMID:          100,
		VCPU:          2,
		MemoryMiB:     2048,
		StoragePool:   "local-lvm",
		NetworkBridge: "vmbr0",
		Serial:        "ABC123",
	})
	assert.NoError(t, err)
	assert.Contains(t, gotQuery, "smbios1")
	assert.Contains(t, gotQuery, "serial%3DQUJDMTIz") // base64 of "ABC123"
	assert.Contains(t, gotQuery, "base64%3D1")
}

func TestCreateVMFromScratch_SerialHyphenStripped(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	assert.NoError(t, err)

	err = c.createVMFromScratch(context.Background(), VMConfig{
		Name:          "test-vm",
		VMID:          100,
		VCPU:          2,
		MemoryMiB:     2048,
		StoragePool:   "local-lvm",
		NetworkBridge: "vmbr0",
		Serial:        "worker-vm",
	})
	assert.NoError(t, err)
	assert.Contains(t, gotQuery, "smbios1")
	assert.Contains(t, gotQuery, "serial%3Dd29ya2VyLXZt") // base64 of "worker-vm"
	assert.Contains(t, gotQuery, "base64%3D1")
}

func TestCreateVMFromScratch_DefaultCPUType(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	assert.NoError(t, err)

	err = c.createVMFromScratch(context.Background(), VMConfig{
		Name:          "test-vm",
		VMID:          100,
		VCPU:          2,
		MemoryMiB:     2048,
		StoragePool:   "local-lvm",
		NetworkBridge: "vmbr0",
	})
	assert.NoError(t, err)
	assert.Contains(t, gotQuery, "cpu=host") // default
}

func TestCreateVMFromScratch_CustomCPUType(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	assert.NoError(t, err)

	err = c.createVMFromScratch(context.Background(), VMConfig{
		Name:          "test-vm",
		VMID:          100,
		VCPU:          2,
		MemoryMiB:     2048,
		StoragePool:   "local-lvm",
		NetworkBridge: "vmbr0",
		CPUType:       "kvm64",
	})
	assert.NoError(t, err)
	assert.Contains(t, gotQuery, "cpu=kvm64")
}

func TestStartVM(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	require.NoError(t, err)

	err = c.startVM(context.Background(), "pve", 200)
	assert.NoError(t, err)
	assert.Equal(t, "POST", gotMethod)
	assert.Equal(t, "/api2/json/nodes/pve/qemu/200/status/start", gotPath)
}

func TestStopVM(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	require.NoError(t, err)

	err = c.StopVM(context.Background(), 200)
	assert.NoError(t, err)
	assert.Equal(t, "POST", gotMethod)
	assert.Equal(t, "/api2/json/nodes/pve/qemu/200/status/stop", gotPath)
}

func TestDeleteVM(t *testing.T) {
	var methods []string
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		paths = append(paths, r.URL.Path)
		if strings.Contains(r.URL.Path, "/status/current") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]string{"status": "stopped"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	require.NoError(t, err)

	err = c.DeleteVM(context.Background(), 300)
	assert.NoError(t, err)
	// DeleteVM resolves the VM's node (GET cluster resources), shuts it down
	// gracefully, polls status until stopped, then deletes it.
	require.Len(t, methods, 4)
	assert.Equal(t, "GET", methods[0])    // node lookup
	assert.Equal(t, "POST", methods[1])   // graceful shutdown
	assert.Equal(t, "GET", methods[2])    // status poll
	assert.Equal(t, "DELETE", methods[3]) // delete
	assert.Contains(t, paths[1], "/status/shutdown")
	assert.Equal(t, "/api2/json/nodes/pve/qemu/300", paths[3])
}

func TestDeleteVM_HardStopsWhenShutdownIgnored(t *testing.T) {
	var methods []string
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		paths = append(paths, r.URL.Path)
		if strings.Contains(r.URL.Path, "/status/current") {
			// VM refuses to shut down; always reports running.
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]string{"status": "running"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	oldShutdownWait, oldHardStopWait := vmShutdownWait, vmHardStopWait
	vmShutdownWait, vmHardStopWait = 50*time.Millisecond, 50*time.Millisecond
	defer func() { vmShutdownWait, vmHardStopWait = oldShutdownWait, oldHardStopWait }()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	require.NoError(t, err)

	err = c.DeleteVM(context.Background(), 300)
	assert.NoError(t, err)
	require.Len(t, methods, 6)
	assert.Equal(t, "GET", methods[0])    // node lookup
	assert.Equal(t, "POST", methods[1])   // graceful shutdown (ignored)
	assert.Equal(t, "POST", methods[3])   // hard stop
	assert.Equal(t, "DELETE", methods[5]) // delete
	assert.Contains(t, paths[1], "/status/shutdown")
	assert.Contains(t, paths[3], "/status/stop")
	assert.Equal(t, "/api2/json/nodes/pve/qemu/300", paths[5])
}

func TestDeleteVM_ResolvesNodeFromCluster(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/api2/json/cluster/resources" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]interface{}{
					{"vmid": 300, "name": "vm-x", "node": "pve2", "status": "running", "type": "qemu", "template": 0, "tags": "talos"},
				},
			})
			return
		}
		if strings.Contains(r.URL.Path, "/status/current") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]string{"status": "stopped"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	require.NoError(t, err)
	c.node = "pve1" // simulate ResolveNode pinning a different node

	err = c.DeleteVM(context.Background(), 300)
	assert.NoError(t, err)
	assert.Equal(t, "/api2/json/nodes/pve2/qemu/300/status/shutdown", paths[1])
	assert.Equal(t, "/api2/json/nodes/pve2/qemu/300", paths[3])
}

func TestCreateVM_UsesConfigNode(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.Contains(r.URL.Path, "/agent/network-get-interfaces") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]interface{}{
					{"name": "eth0", "ip-addresses": []map[string]interface{}{
						{"ip-address": "10.0.0.5", "ip-address-type": "ipv4"},
					}},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = c.CreateVM(ctx, VMConfig{
		Name:          "spread-vm",
		Node:          "pve2",
		VMID:          500,
		VCPU:          2,
		MemoryMiB:     2048,
		DiskGiB:       10,
		StoragePool:   "local-lvm",
		NetworkBridge: "vmbr0",
	})
	assert.NoError(t, err)
	require.Len(t, paths, 3)
	assert.Equal(t, "/api2/json/nodes/pve2/qemu", paths[0])
	assert.Equal(t, "/api2/json/nodes/pve2/qemu/500/status/start", paths[1])
	assert.Contains(t, paths[2], "/api2/json/nodes/pve2/qemu/500/agent/network-get-interfaces")
}

func TestWaitForIP_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]interface{}{
				{
					"name": "eth0",
					"ip-addresses": []map[string]interface{}{
						{"ip-address": "10.0.0.5", "ip-address-type": "ipv4"},
					},
				},
			},
		})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	require.NoError(t, err)

	ip, err := c.waitForIP(context.Background(), "pve", 100, 10*time.Second)
	assert.NoError(t, err)
	assert.Equal(t, "10.0.0.5", ip)
}

func TestWaitForIP_SkipsLoopback(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			// First call: only loopback
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]interface{}{
					{
						"name": "lo",
						"ip-addresses": []map[string]interface{}{
							{"ip-address": "127.0.0.1", "ip-address-type": "ipv4"},
						},
					},
				},
			})
			return
		}
		// Second call: real interface
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]interface{}{
				{
					"name": "eth0",
					"ip-addresses": []map[string]interface{}{
						{"ip-address": "10.0.0.5", "ip-address-type": "ipv4"},
					},
				},
			},
		})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	require.NoError(t, err)

	ip, err := c.waitForIP(context.Background(), "pve", 100, 30*time.Second)
	assert.NoError(t, err)
	assert.Equal(t, "10.0.0.5", ip)
	assert.GreaterOrEqual(t, callCount, 2)
}

func TestWaitForIP_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, err = c.waitForIP(ctx, "pve", 100, 5*time.Minute)
	assert.Error(t, err)
}

func TestWaitForTask_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]string{"status": "stopped"},
		})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	require.NoError(t, err)

	err = c.waitForTask(context.Background(), "pve", "UPID:sometask")
	assert.NoError(t, err)
}

func TestWaitForTask_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]string{"status": "running"},
		})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err = c.waitForTask(ctx, "pve", "UPID:sometask")
	assert.Error(t, err)
}

func TestCloneVM(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, fmt.Sprintf("%s %s", r.Method, r.URL.Path))
		switch {
		case r.Method == "POST" && r.URL.Path == "/api2/json/nodes/pve/qemu/900/clone":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": "UPID:clone-task",
			})
		case r.Method == "GET" && r.URL.Path == "/api2/json/nodes/pve/tasks/UPID:clone-task":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]string{"status": "stopped"},
			})
		case r.Method == "PUT":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	require.NoError(t, err)

	err = c.cloneVM(context.Background(), VMConfig{
		Name:          "cloned-vm",
		VMID:          100,
		TemplateID:    900,
		VCPU:          4,
		MemoryMiB:     4096,
		DiskGiB:       50,
		StoragePool:   "local-lvm",
		NetworkBridge: "vmbr0",
		MACAddress:    "AA:BB:CC:DD:EE:FF",
	})
	assert.NoError(t, err)
	require.Len(t, calls, 4)
	assert.Contains(t, calls[0], "/cluster/resources")
	assert.Contains(t, calls[1], "/clone")
	assert.Contains(t, calls[2], "/tasks/")
	assert.Equal(t, "PUT /api2/json/nodes/pve/qemu/100/config", calls[3])
}

func TestCloneVM_UsesConfigNode(t *testing.T) {
	var gotPaths []string
	gotForm := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, fmt.Sprintf("%s %s", r.Method, r.URL.Path))
		switch {
		case r.Method == "GET" && r.URL.Path == "/api2/json/cluster/resources":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": []map[string]interface{}{
					{"vmid": 900, "name": "tmpl", "node": "pve0", "template": 1, "type": "qemu", "status": "stopped"},
				},
			})
		case r.Method == "POST" && r.URL.Path == "/api2/json/nodes/pve0/qemu/900/clone":
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for k, v := range body {
				gotForm[k] = fmt.Sprint(v)
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": "UPID:clone-task"})
		case r.Method == "GET" && r.URL.Path == "/api2/json/nodes/pve0/tasks/UPID:clone-task":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]string{"status": "stopped"}})
		case r.Method == "PUT" && r.URL.Path == "/api2/json/nodes/pve2/qemu/901/config":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	require.NoError(t, err)

	err = c.cloneVM(context.Background(), VMConfig{
		Name:          "clone-x",
		Node:          "pve2",
		VMID:          901,
		TemplateID:    900,
		VCPU:          2,
		MemoryMiB:     2048,
		DiskGiB:       10,
		StoragePool:   "local-lvm",
		NetworkBridge: "vmbr0",
	})
	require.NoError(t, err)
	require.Len(t, gotPaths, 4)
	assert.Equal(t, "GET /api2/json/cluster/resources", gotPaths[0])
	assert.Equal(t, "POST /api2/json/nodes/pve0/qemu/900/clone", gotPaths[1])
	assert.Equal(t, "PUT /api2/json/nodes/pve2/qemu/901/config", gotPaths[3])
	assert.Equal(t, "901", gotForm["newid"])
	assert.Equal(t, "pve2", gotForm["target"])
}

func TestCreateVM_Scratch(t *testing.T) {
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = c.CreateVM(ctx, VMConfig{
		Name:          "new-vm",
		VMID:          500,
		VCPU:          4,
		MemoryMiB:     8192,
		DiskGiB:       100,
		StoragePool:   "local-lvm",
		NetworkBridge: "vmbr0",
		MACAddress:    "AA:BB:CC:DD:EE:FF",
	})
	// createVMFromScratch + startVM + at least one agent poll before context cancel
	assert.Error(t, err) // timeout waiting for IP
	assert.GreaterOrEqual(t, len(gotPaths), 2)
	assert.Contains(t, gotPaths[0], "/qemu") // create from scratch
	assert.Contains(t, gotPaths[1], "/status/start")
}

func TestDo_TokenAuthHeaders(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": json.RawMessage(`"ok"`),
		})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "my-secret", "pve", true)
	require.NoError(t, err)

	_, err = c.do(context.Background(), "GET", "/api2/json/version", nil)
	assert.NoError(t, err)
	assert.Equal(t, "PVEAPIToken=user@realm!tok=my-secret", gotAuth)
}

func TestDo_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve", true)
	require.NoError(t, err)

	_, err = c.do(context.Background(), "GET", "/api2/json/version", nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestListVMs(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]interface{}{
				{"vmid": 100, "name": "vm-a", "node": "pve1", "status": "running", "type": "qemu", "template": 0, "tags": "talos,worker"},
				{"vmid": 200, "name": "vm-b", "node": "pve2", "status": "stopped", "type": "lxc", "template": 1, "tags": "talos;gpu"},
				{"vmid": 300, "name": "vm-c", "node": "pve3", "status": "running", "type": "qemu", "template": 0, "tags": ""},
			},
		})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "", true)
	require.NoError(t, err)

	vms, err := c.ListVMs(context.Background())
	require.NoError(t, err)
	require.Len(t, vms, 3)
	assert.Equal(t, "/api2/json/cluster/resources?type=vm", gotPath)
	assert.Equal(t, 100, vms[0].VMID)
	assert.Equal(t, "vm-a", vms[0].Name)
	assert.Equal(t, "pve1", vms[0].Node)
	assert.Equal(t, "qemu", vms[0].Type)
	assert.Equal(t, 0, vms[0].Template)
	assert.Equal(t, "talos,worker", vms[0].Tags)
	assert.Equal(t, "talos;gpu", vms[1].Tags)
	assert.Equal(t, 1, vms[1].Template)
}
