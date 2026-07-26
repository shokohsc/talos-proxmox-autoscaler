package proxmox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
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
			json.NewEncoder(w).Encode(map[string]interface{}{
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
		w.Write([]byte(`{"errors":{"username":"invalid"}}`))
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
			json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]string{
					"ticket":              "PVEticket",
					"CSRFPreventionToken": "csrf",
				},
			})
			return
		}
		// For the actual API call, verify auth headers
		cookie := r.Header.Get("Cookie")
		csrf := r.Header.Get("CSRFPreventionToken")
		if cookie != "PVEAuthCookie=PVEticket" || csrf != "csrf" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"data":null}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
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
		json.NewEncoder(w).Encode(map[string]interface{}{
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
		json.NewEncoder(w).Encode(map[string]interface{}{
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
		t.Fatal("server should not be called when node is configured")
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "", "", "user@realm!tok", "secret", "pve1", true)
	assert.NoError(t, err)

	node, err := c.GetNode(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, "pve1", node)
}

func TestGetNode_AutoDiscover(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
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
		json.NewEncoder(w).Encode(map[string]interface{}{
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

func TestPasswordAuth_CachesTicket(t *testing.T) {
	loginCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api2/json/access/ticket" {
			loginCount++
			json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]string{
					"ticket":              "PVEticket",
					"CSRFPreventionToken": "csrf",
				},
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": json.RawMessage(`"ok"`),
		})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "root@pam", "pass", "", "", "pve", true)
	assert.NoError(t, err)

	// First call triggers login
	_, err = c.do(context.Background(), "GET", "/api2/json/version", nil)
	assert.NoError(t, err)
	assert.Equal(t, 1, loginCount)

	// Second call reuses ticket
	_, err = c.do(context.Background(), "GET", "/api2/json/version", nil)
	assert.NoError(t, err)
	assert.Equal(t, 1, loginCount)
}

func TestCreateVMFromScratch_Tags(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
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
		json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
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
		json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
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
		json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
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
