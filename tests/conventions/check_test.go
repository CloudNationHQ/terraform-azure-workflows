package conventions

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create parent directory for %q: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
	}
	return dir
}

// ids renders findings as "RULEID@file:line".
func ids(findings []Finding) []string {
	result := make([]string, 0, len(findings))
	for _, f := range findings {
		result = append(result, f.RuleID+"@"+f.File+":"+strconv.Itoa(f.Line))
	}
	return result
}

const countResource = `resource "azurerm_resource_group" "x" {
  count = 1
  name = "x"
}`

func TestRules(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		repo  string
		want  []string // exact findings, in order
	}{
		// WAM001
		{name: "WAM001 any", files: map[string]string{"variables.tf": `variable "x" { type = any }`},
			want: []string{"WAM001@variables.tf:1"}},
		{name: "WAM001 map any", files: map[string]string{"variables.tf": `variable "x" {
  type = map(any)
}`}, want: []string{"WAM001@variables.tf:2"}},
		{name: "WAM001 object any", files: map[string]string{"variables.tf": `variable "x" {
  type = object({
    a = any
  })
}`}, want: []string{"WAM001@variables.tf:3"}},
		{name: "WAM001 map string", files: map[string]string{"variables.tf": `variable "x" { type = map(string) }`}},

		// WAM002
		{name: "WAM002 resource count", files: map[string]string{"main.tf": countResource},
			want: []string{"WAM002@main.tf:2"}},
		{name: "WAM002 module count", files: map[string]string{"main.tf": `module "x" {
  source = "./modules/x"
  count = 1
}`}, want: []string{"WAM002@main.tf:3"}},
		{name: "WAM002 for_each", files: map[string]string{"main.tf": `resource "azurerm_resource_group" "x" {
  for_each = { this = 1 }
  name = "x"
}`}},

		// WAM003 and WAM014
		{name: "WAM003 optional true", files: map[string]string{"variables.tf": `variable "x" {
  type = object({ enabled = optional(bool, true) })
}`}, want: []string{"WAM003@variables.tf:2"}},
		{name: "WAM003 optional false and no default", files: map[string]string{"variables.tf": `variable "x" {
  type = object({
    enabled = optional(bool, false)
    other = optional(bool)
  })
}`}},
		{name: "WAM014 purge protection default", files: map[string]string{"variables.tf": `variable "x" {
  type = object({ purge_protection_enabled = optional(bool, false) })
}`}, want: []string{"WAM014@variables.tf:2"}},
		{name: "WAM014 public network true also WAM003", files: map[string]string{"variables.tf": `variable "x" {
  type = object({ public_network_access_enabled = optional(bool, true) })
}`}, want: []string{"WAM003@variables.tf:2", "WAM014@variables.tf:2"}},
		{name: "WAM014 no default", files: map[string]string{"variables.tf": `variable "x" {
  type = object({ purge_protection_enabled = optional(bool) })
}`}},

		// WAM004
		{name: "WAM004 string fallback", files: map[string]string{"main.tf": `resource "azurerm_resource_group" "x" {
  name = try(var.x.name, "foo")
}`}, want: []string{"WAM004@main.tf:2"}},
		{name: "WAM004 empty object fallback", files: map[string]string{"main.tf": `resource "azurerm_resource_group" "x" {
  name = "x"
  tags = try(var.x.a, {})
}`}, want: []string{"WAM004@main.tf:3"}},
		{name: "WAM004 null fallback", files: map[string]string{"main.tf": `resource "azurerm_resource_group" "x" {
  name = try(var.x.a, null)
}`}},
		{name: "WAM004 only main.tf", files: map[string]string{"locals.tf": `locals {
  name = try(var.x.name, "foo")
}`}},

		// WAM005
		{name: "WAM005 required_version lower bound", files: map[string]string{"terraform.tf": `terraform {
  required_version = ">= 1.3"
}`}, want: []string{"WAM005@terraform.tf:2"}},
		{name: "WAM005 provider lower bound", files: map[string]string{"terraform.tf": `terraform {
  required_providers {
    azurerm = {
      source = "hashicorp/azurerm"
      version = ">= 4.0"
    }
  }
}`}, want: []string{"WAM005@terraform.tf:5"}},
		{name: "WAM005 pessimistic", files: map[string]string{"terraform.tf": `terraform {
  required_version = "~> 1.0"
  required_providers {
    azurerm = {
      source = "hashicorp/azurerm"
      version = "~> 4.0"
    }
  }
}`}},
		{name: "WAM005 combined constraint", files: map[string]string{"terraform.tf": `terraform {
  required_providers {
    azurerm = {
      source = "hashicorp/azurerm"
      version = "~> 4.0, >= 4.1"
    }
  }
}`}, want: []string{"WAM005@terraform.tf:5"}},
		{name: "WAM005 only terraform.tf", files: map[string]string{"versions.tf": `terraform {
  required_version = ">= 1.3"
}`}},

		// WAM006
		{name: "WAM006 user assigned identity", files: map[string]string{"main.tf": `resource "azurerm_user_assigned_identity" "x" {
  name = "x"
}`}, want: []string{"WAM006@main.tf:1"}},
		{name: "WAM006 uai repo exempt", repo: "terraform-azure-uai", files: map[string]string{"main.tf": `resource "azurerm_user_assigned_identity" "x" {
  name = "x"
}`}},

		// WAM007
		{name: "WAM007 no comment", files: map[string]string{"main.tf": `resource "azapi_resource" "x" {}`},
			want: []string{"WAM007@main.tf:1"}},
		{name: "WAM007 section header is not enough", files: map[string]string{"main.tf": `# container app job
resource "azapi_resource" "x" {}`}, want: []string{"WAM007@main.tf:2"}},
		{name: "WAM007 explanation above", files: map[string]string{"main.tf": `# azurerm does not support the job schedule yet
resource "azapi_resource" "x" {}`}},
		{name: "WAM007 explanation inside", files: map[string]string{"main.tf": `resource "azapi_resource" "x" {
  # azurerm does not support the job schedule yet
  name = "x"
}`}},
		{name: "WAM007 blank line breaks the run", files: map[string]string{"main.tf": `# azurerm does not support the job schedule yet

resource "azapi_resource" "x" {}`}, want: []string{"WAM007@main.tf:3"}},

		// WAM008
		{name: "WAM008 uncommented lifecycle", files: map[string]string{"main.tf": `resource "azurerm_resource_group" "x" {
  name = "x"
  lifecycle {
    ignore_changes = [tags]
  }
}`}, want: []string{"WAM008@main.tf:3"}},
		{name: "WAM008 trailing comment", files: map[string]string{"main.tf": `resource "azurerm_resource_group" "x" {
  name = "x"
  lifecycle { # tags are managed by an external policy
    ignore_changes = [tags]
  }
}`}},
		{name: "WAM008 comment inside", files: map[string]string{"main.tf": `resource "azurerm_resource_group" "x" {
  name = "x"
  lifecycle {
    # tags are managed by an external policy
    ignore_changes = [tags]
  }
}`}},

		// WAM009
		{name: "WAM009 uncommented", files: map[string]string{"main.tf": `resource "azurerm_resource_group" "x" {
  name = "x"
  depends_on = [azurerm_x.y]
}`}, want: []string{"WAM009@main.tf:3"}},
		{name: "WAM009 two comment lines", files: map[string]string{"main.tf": `resource "azurerm_resource_group" "x" {
  name = "x"
  # the role assignment must exist first
  # its effect is not visible as an attribute
  depends_on = [azurerm_x.y]
}`}},
		{name: "WAM009 trailing comment on previous code line", files: map[string]string{"main.tf": `resource "azurerm_resource_group" "x" {
  name = "a" # some long comment here ok
  depends_on = [azurerm_x.y]
}`}, want: []string{"WAM009@main.tf:3"}},

		// WAM010
		{name: "WAM010 moved in main.tf", files: map[string]string{"main.tf": `moved {
  from = azurerm_resource_group.old
  to = azurerm_resource_group.new
}`}, want: []string{"WAM010@main.tf:1"}},
		{name: "WAM010 versioned file", files: map[string]string{"moved_v4_to_v5.tf": `moved {
  from = azurerm_resource_group.old
  to = azurerm_resource_group.new
}`}},
		{name: "WAM010 unversioned file", files: map[string]string{"moved.tf": `moved {
  from = azurerm_resource_group.old
  to = azurerm_resource_group.new
}`}, want: []string{"WAM010@moved.tf:1"}},

		// WAM011
		{name: "WAM011 quoted default key", files: map[string]string{"main.tf": `resource "azurerm_resource_group" "x" {
  for_each = var.x != null ? { "default" = var.x } : {}
  name = "x"
}`}, want: []string{"WAM011@main.tf:2"}},
		{name: "WAM011 this key", files: map[string]string{"main.tf": `resource "azurerm_resource_group" "x" {
  for_each = var.x != null ? { "this" = var.x } : {}
  name = "x"
}`}},
		{name: "WAM011 bare default key", files: map[string]string{"main.tf": `resource "azurerm_resource_group" "x" {
  for_each = { default = 1 }
  name = "x"
}`}, want: []string{"WAM011@main.tf:2"}},

		// WAM012
		{name: "WAM012 forbidden names", files: map[string]string{"variables.tf": `variable "naming" { type = string }
variable "config" { type = string }
variable "environment" { type = string }
variable "instance" { type = string }`}, want: []string{
			"WAM012@variables.tf:1", "WAM012@variables.tf:2", "WAM012@variables.tf:3", "WAM012@variables.tf:4"}},
		{name: "WAM012 root only", files: map[string]string{"modules/sub/variables.tf": `variable "naming" { type = string }`}},

		// WAM013
		{name: "WAM013 default", files: map[string]string{"variables.tf": `variable "foo" {
  type = string
  default = "x"
}`}, want: []string{"WAM013@variables.tf:3"}},
		{name: "WAM013 value defaults", files: map[string]string{"variables.tf": `variable "a" {
  type = bool
  default = false
}
variable "b" {
  type = map(string)
  default = { x = "y" }
}`}, want: []string{"WAM013@variables.tf:3", "WAM013@variables.tf:7"}},
		{name: "WAM013 empty defaults allowed", files: map[string]string{"variables.tf": `variable "a" {
  type = map(string)
  default = {}
}
variable "b" {
  type = list(string)
  default = []
}
variable "c" {
  type = string
  default = null
}`}},
		{name: "WAM013 use_existing false allowed, true flagged", files: map[string]string{"variables.tf": `variable "use_existing_vnet" {
  type = bool
  default = false
}
variable "use_existing_zone" {
  type = bool
  default = true
}`}, want: []string{"WAM013@variables.tf:7"}},
		{name: "WAM013 location default allowed", files: map[string]string{"variables.tf": `variable "location" {
  type = string
  default = null
}`}},
		{name: "WAM013 root only", files: map[string]string{"modules/sub/variables.tf": `variable "foo" {
  type = string
  default = "x"
}`}},

		// ignores
		{name: "ignore above", files: map[string]string{"main.tf": `resource "azurerm_resource_group" "x" {
  # wam-lint-ignore WAM002 legacy, removed in v5
  count = 1
  name = "x"
}`}},
		{name: "ignore trailing", files: map[string]string{"main.tf": `resource "azurerm_resource_group" "x" {
  count = 1 # wam-lint-ignore WAM002 legacy, removed in v5
  name = "x"
}`}},
		{name: "ignore skips blank and comment lines", files: map[string]string{"main.tf": `resource "azurerm_resource_group" "x" {
  # wam-lint-ignore WAM002 legacy, removed in v5

  # keep until the next major release
  count = 1
  name = "x"
}`}},
		{name: "ignore without reason", files: map[string]string{"main.tf": `resource "azurerm_resource_group" "x" {
  # wam-lint-ignore WAM002
  count = 1
  name = "x"
}`}, want: []string{"WAM000@main.tf:2", "WAM002@main.tf:3"}},
		{name: "ignore unknown rule", files: map[string]string{"main.tf": `resource "azurerm_resource_group" "x" {
  # wam-lint-ignore WAM999 legacy, removed in v5
  count = 1
  name = "x"
}`}, want: []string{"WAM000@main.tf:2", "WAM002@main.tf:3"}},
		{name: "ignore several rules", files: map[string]string{"main.tf": `module "x" {
  source = "./x"
  # wam-lint-ignore WAM002,WAM009 legacy, removed in v5
  count = 1
  # wam-lint-ignore WAM002,WAM009 legacy, removed in v5
  depends_on = [azurerm_x.y]
}`}},
		{name: "ignore is rule specific", files: map[string]string{"main.tf": `resource "azurerm_resource_group" "x" {
  # wam-lint-ignore WAM013 legacy, removed in v5
  count = 1
  name = "x"
}`}, want: []string{"WAM002@main.tf:3"}},

		// discovery and sorting
		{name: "skips examples, tests and .terraform", files: map[string]string{
			"examples/default/main.tf":   countResource,
			"tests/main.tf":              countResource,
			".terraform/main.tf":         countResource,
			"modules/a/examples/main.tf": countResource,
		}},
		{name: "checks nested sub-modules", files: map[string]string{"modules/a/b/main.tf": countResource},
			want: []string{"WAM002@modules/a/b/main.tf:2"}},
		{name: "only .tf files", files: map[string]string{"main.txt": countResource}},
		{name: "empty dir", files: map[string]string{}},
		{name: "sorted by file, line, rule", files: map[string]string{
			"z.tf": `resource "azurerm_resource_group" "x" {
  count = 1
  depends_on = [azurerm_x.y]
}`,
			"modules/a/main.tf": countResource,
			"a.tf": `variable "x" {
  type = object({ public_network_access_enabled = optional(bool, true) })
}
variable "foo" {
  default = "x"
}`,
		}, want: []string{
			"WAM003@a.tf:2", "WAM014@a.tf:2", "WAM013@a.tf:5",
			"WAM002@modules/a/main.tf:2", "WAM002@z.tf:2", "WAM009@z.tf:3",
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings, err := Check(writeModule(t, tc.files), Options{RepoName: tc.repo})
			if err != nil {
				t.Fatalf("Check() error: %v", err)
			}
			got := strings.Join(ids(findings), "\n")
			want := strings.Join(tc.want, "\n")
			if got != want {
				t.Errorf("findings:\n got: %v\nwant: %v", ids(findings), tc.want)
			}
		})
	}
}

func TestRulesDocInSync(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "lint", "CONVENTIONS.md"))
	if err != nil {
		t.Fatalf("read lint/CONVENTIONS.md: %v", err)
	}
	if strings.ReplaceAll(string(data), "\r\n", "\n") != RulesMarkdown() {
		t.Error("lint/CONVENTIONS.md is out of date; run: cd tests && go run ./cmd/wamlint -rules > ../lint/CONVENTIONS.md")
	}
}
