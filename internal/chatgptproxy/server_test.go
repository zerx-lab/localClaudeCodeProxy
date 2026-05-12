package chatgptproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestServerStartsWithModelsRoute(t *testing.T) {
	server := NewServer(NewCredentialManager(), nil)
	if err := server.Start(Config{Host: "127.0.0.1", Port: 0}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Stop(ctx); err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
	}()

	baseURL := "http://" + server.ListenAddr()
	for _, path := range []string{"/healthz", "/v1/models"} {
		resp, err := http.Get(baseURL + path)
		if err != nil {
			t.Fatalf("GET %s error = %v", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", path, resp.StatusCode)
		}
	}

	resp, err := http.Get(baseURL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models error = %v", err)
	}
	defer resp.Body.Close()
	var payload struct {
		Object string `json:"object"`
		Data   []any  `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode /v1/models: %v", err)
	}
	if payload.Object != "list" || len(payload.Data) == 0 {
		t.Fatalf("unexpected /v1/models payload: object=%q len=%d", payload.Object, len(payload.Data))
	}
}
