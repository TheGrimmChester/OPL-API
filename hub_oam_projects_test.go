package main

import (
	"encoding/json"
	"net/url"
	"testing"
)

func TestOAMProjectsTargetForwardsProduct(t *testing.T) {
	q := url.Values{}
	q.Set("organization_id", "acme")
	q.Set("product", "opl")
	got := oamProjectsTarget("http://oam:8090/", q)
	want := "http://oam:8090/api/projects?organization_id=acme&product=opl"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestOAMProjectsTargetDropsAllOrg(t *testing.T) {
	q := url.Values{}
	q.Set("organization_id", "all")
	q.Set("product", "opl")
	got := oamProjectsTarget("http://oam:8090", q)
	want := "http://oam:8090/api/projects?product=opl"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestAliasDirectoryIDsAddsProjectID(t *testing.T) {
	raw := []byte(`{"projects":[{"id":"web","name":"Web"}]}`)
	out := aliasDirectoryIDs(raw, "projects", "project_id")
	var payload map[string]interface{}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	list := payload["projects"].([]interface{})
	row := list[0].(map[string]interface{})
	if row["project_id"] != "web" || row["id"] != "web" {
		t.Fatalf("alias missing: %#v", row)
	}
}
