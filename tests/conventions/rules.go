// Package conventions checks Terraform modules against the mechanical rules of
// the WAM standard (wam-support/CLAUDE.md). Each check cites the rule number it
// enforces in Rule.Standard.
package conventions

type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

type Rule struct {
	ID, Standard, Title string
	Severity            Severity
}

type Finding struct {
	RuleID   string   `json:"rule"`
	Severity Severity `json:"severity"`
	File     string   `json:"file"`
	Line     int      `json:"line"`
	Message  string   `json:"message"`
}

type Options struct {
	RepoName string
}

const StandardURL = "https://github.com/CloudNationHQ/wam-support/blob/main/CLAUDE.md"

var catalogue = []Rule{
	{ID: "WAM000", Standard: "-", Severity: SeverityError, Title: "wam-lint-ignore comment is invalid"},
	{ID: "WAM001", Standard: "7", Severity: SeverityError, Title: "no `any` in variable types"},
	{ID: "WAM002", Standard: "9", Severity: SeverityError, Title: "for_each only, never count"},
	{ID: "WAM003", Standard: "7", Severity: SeverityError, Title: "no optional(bool, true)"},
	{ID: "WAM004", Standard: "7, 8", Severity: SeverityWarning, Title: "no try() defaulting in main.tf"},
	{ID: "WAM005", Standard: "3", Severity: SeverityError, Title: "pessimistic (~>) version constraints"},
	{ID: "WAM006", Standard: "17", Severity: SeverityError, Title: "no user-assigned identity inside a module"},
	{ID: "WAM007", Standard: "4", Severity: SeverityError, Title: "azapi needs an explaining comment"},
	{ID: "WAM008", Standard: "12", Severity: SeverityError, Title: "lifecycle needs an explaining comment"},
	{ID: "WAM009", Standard: "12", Severity: SeverityError, Title: "depends_on needs an explaining comment"},
	{ID: "WAM010", Standard: "26", Severity: SeverityError, Title: "moved blocks only in moved_vX_to_vY.tf"},
	{ID: "WAM011", Standard: "1, 10", Severity: SeverityWarning, Title: `sentinel key is "this", not "default"`},
	{ID: "WAM012", Standard: "5", Severity: SeverityError, Title: "no config, instance, environment or naming variable"},
	{ID: "WAM013", Standard: "6", Severity: SeverityError, Title: "only location, resource_group_name and tags have value defaults; other inputs default to null, {} or [] (use_existing_* to false)"},
	{ID: "WAM014", Standard: "7", Severity: SeverityError, Title: "no default on security flags"},
}

// Rules returns the catalogue ordered by ID, without exposing its backing slice.
func Rules() []Rule {
	return append([]Rule(nil), catalogue...)
}

func RuleByID(id string) (Rule, bool) {
	for _, rule := range catalogue {
		if rule.ID == id {
			return rule, true
		}
	}
	return Rule{}, false
}
