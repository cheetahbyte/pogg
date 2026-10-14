package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstanceUpdateRefusesAddressChangeForBoundApps(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PGPLEASE_CONFIG_DIR", dir)
	files := map[string]string{
		"state.json":   `{"version":1,"defaultInstance":"local","instances":{"local":{"host":"127.0.0.1","port":5432,"admin":"postgres","sslmode":"prefer"}},"apps":{"shop":{"instance":"local","databases":{},"users":{}}}}` + "\n",
		"secrets.json": `{"instance/local/admin":"stored-password"}` + "\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"--host", "192.0.2.1"}, {"--port", "6543"}} {
		var out, errOut bytes.Buffer
		err := Run(context.Background(), append([]string{"instance", "update", "local"}, args...), strings.NewReader(""), &out, &errOut)
		if err == nil || !strings.Contains(err.Error(), "shop") || !strings.Contains(err.Error(), "--force") {
			t.Fatalf("expected bound app refusal for %v, got %v", args, err)
		}
	}
	for name, content := range files {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != content {
			t.Fatalf("refused address change modified %s", name)
		}
	}
}
