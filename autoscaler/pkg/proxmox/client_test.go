package proxmox

import (
	"context"
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

func TestPasswordAuth_EmptyTicketReturnsError(t *testing.T) {
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
	assert.Equal(t, "", client.ticket)

	_, err = client.do(context.Background(), "GET", "/api2/json/version", nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "password auth requires login")
}
