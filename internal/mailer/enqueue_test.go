package mailer

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestBuildEmailTask(t *testing.T) {
	p, err := buildEmailTask("magic_link", "a@b.com", map[string]any{"Link": "x"}, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != "email" {
		t.Fatalf("kind=%q, want email", p.Kind)
	}
	var task emailTask
	if err := json.Unmarshal(p.Data, &task); err != nil {
		t.Fatal(err)
	}
	if task.Template != "magic_link" || task.To != "a@b.com" || task.Vars["Link"] != "x" {
		t.Fatalf("bad task: %+v", task)
	}
}
