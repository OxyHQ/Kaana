// Package commercial holds the reviewed commercial facts of Kaana's deployments
// (`configs/deployment-commercial.json`): who a deployment may be served to and
// on what basis. Kaana publishes them as the optional `commercial` member of
// every deployment descriptor it serves to Oxy (contract set 4.4.0); Oxy reads
// them and decides nothing here.
//
// The file is keyed by exact deployment id and lists ONLY deployments that are
// sold. A deployment that is absent is not sold and its descriptor carries no
// `commercial`: nothing is defaulted, inherited or inferred, so a deployment the
// publisher mints under a new observation date (its id carries the date) is
// unsold until a reviewed commit adds it.
package commercial

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/OxyHQ/Kaana/internal/contract"
)

// SchemaVersion is the file's own format version.
const SchemaVersion = 1

type entry struct {
	DeploymentID contract.DeploymentID         `json:"deploymentId"`
	Commercial   contract.DeploymentCommercial `json:"commercial"`
}

type file struct {
	Comment       json.RawMessage `json:"$comment"`
	SchemaVersion int             `json:"schemaVersion"`
	Deployments   []entry         `json:"deployments"`
}

// Table is the parsed file. The zero value and nil are the empty table: no
// deployment is sold.
type Table struct {
	byID map[contract.DeploymentID]contract.DeploymentCommercial
}

// Load reads and validates the file. An empty path is the empty table.
func Load(path string) (*Table, error) {
	if strings.TrimSpace(path) == "" {
		return &Table{}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("commercial: reading the deployment commercial file: %w", err)
	}
	return Parse(raw)
}

// Parse validates the file: strict fields, schemaVersion 1, a valid `commercial`
// per entry, and entries sorted by deployment id without duplicates, so a
// review diff reads as the change and one id cannot carry two answers.
func Parse(raw []byte) (*Table, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var parsed file
	if err := decoder.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("commercial: the deployment commercial file is not readable: %w", err)
	}
	if parsed.SchemaVersion != SchemaVersion {
		return nil, errors.New("commercial: the deployment commercial file declares no schemaVersion 1")
	}
	if len(parsed.Comment) == 0 {
		return nil, errors.New("commercial: the deployment commercial file carries no $comment provenance note")
	}
	table := &Table{byID: make(map[contract.DeploymentID]contract.DeploymentCommercial, len(parsed.Deployments))}
	var previous contract.DeploymentID
	for index, item := range parsed.Deployments {
		if item.DeploymentID == "" {
			return nil, fmt.Errorf("commercial: deployments[%d] has no deploymentId", index)
		}
		if item.DeploymentID <= previous && index > 0 {
			return nil, fmt.Errorf("commercial: deployments[%d] %q is duplicated or out of order (entries are sorted by deploymentId)", index, item.DeploymentID)
		}
		previous = item.DeploymentID
		if err := item.Commercial.Validate(); err != nil {
			return nil, fmt.Errorf("commercial: deployments[%d] %q: %w", index, item.DeploymentID, err)
		}
		table.byID[item.DeploymentID] = item.Commercial
	}
	return table, nil
}

// Len is how many deployments the file sells.
func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	return len(t.byID)
}

// IDs lists the sold deployment ids in order.
func (t *Table) IDs() []contract.DeploymentID {
	if t == nil {
		return nil
	}
	ids := make([]contract.DeploymentID, 0, len(t.byID))
	for id := range t.byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	return ids
}

// For returns a copy of a deployment's commercial facts, or nil when it is not
// sold. The copy shares nothing with the table.
func (t *Table) For(id contract.DeploymentID) *contract.DeploymentCommercial {
	if t == nil {
		return nil
	}
	found, sold := t.byID[id]
	if !sold {
		return nil
	}
	if found.DedicatedCapacity != nil {
		value := *found.DedicatedCapacity
		found.DedicatedCapacity = &value
	}
	return &found
}
