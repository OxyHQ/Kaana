package httpapi

// LEGACY wire compatibility. Remove once Oxy stops negotiating (OxyHQ/oxy#1809).
//
// Contract set 7.0.0 (#208) deleted scoped execution and Private Auto, and with
// them the negotiated catalogue read POST /internal/v1/models/query and the two
// negotiation keys on POST /internal/v1/deployments/query. The Oxy API in
// production still reads the catalogue and the published deployments through
// that negotiation and refuses any answer that does not echo it, so without
// this shim every grant fails closed (kaana-publication-unavailable).
//
// The shim restores the WIRE only. No scoped or Private Auto row exists any
// more, so a negotiated read is exactly the ordinary projection plus the echoed
// version. Only the two exact old values are accepted, never both at once, and
// nothing else about the old surface returns.

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
)

const (
	legacyScopedExecutionKey                  = "scopedExecutionContractVersion"
	legacyPrivateAutoExecutionKey             = "privateAutoExecutionContractVersion"
	legacyScopedExecutionContractVersion      = "3.6.0"
	legacyPrivateAutoExecutionContractVersion = "3.7.0"
)

// legacyNegotiation is the negotiation a legacy reader sent, echoed verbatim in
// its answer. The zero value is "not negotiated" and encodes as nothing.
type legacyNegotiation struct {
	PrivateAutoExecutionContractVersion string `json:"privateAutoExecutionContractVersion,omitempty"`
	ScopedExecutionContractVersion      string `json:"scopedExecutionContractVersion,omitempty"`
}

func isLegacyNegotiationKey(key string) bool {
	return key == legacyScopedExecutionKey || key == legacyPrivateAutoExecutionKey
}

// accept records one legacy key. A repeated key, a non-string or any value but
// the exact old version is refused.
func (n *legacyNegotiation) accept(key string, raw json.RawMessage) error {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return errors.New("unsupported legacy negotiation")
	}
	switch key {
	case legacyScopedExecutionKey:
		if n.ScopedExecutionContractVersion != "" {
			return errors.New("duplicate scoped version")
		}
		if value != legacyScopedExecutionContractVersion {
			return errors.New("unsupported scoped version")
		}
		n.ScopedExecutionContractVersion = value
	case legacyPrivateAutoExecutionKey:
		if n.PrivateAutoExecutionContractVersion != "" {
			return errors.New("duplicate private Auto version")
		}
		if value != legacyPrivateAutoExecutionContractVersion {
			return errors.New("unsupported private Auto version")
		}
		n.PrivateAutoExecutionContractVersion = value
	default:
		return errors.New("the query contains an unknown field")
	}
	if n.ScopedExecutionContractVersion != "" && n.PrivateAutoExecutionContractVersion != "" {
		return errors.New("private extensions cannot be combined")
	}
	return nil
}

func (n legacyNegotiation) negotiated() bool {
	return n.ScopedExecutionContractVersion != "" || n.PrivateAutoExecutionContractVersion != ""
}

// legacyModelsQueryResponse is GET /internal/v1/models plus the echoed
// negotiation and the deployment descriptors, which a negotiated catalogue
// reader requires (every list price must name a descriptor). `deployments` is
// always present, never omitted.
type legacyModelsQueryResponse struct {
	modelsResponse
	legacyNegotiation
	Deployments []inventory.DeploymentDescriptor `json:"deployments"`
}

// handleLegacyModelsQuery serves POST /internal/v1/models/query: exactly
// {"scopedExecutionContractVersion":"3.6.0"} or
// {"privateAutoExecutionContractVersion":"3.7.0"}, signed. Anything else is
// refused as it always was.
func (s *Server) handleLegacyModelsQuery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	body, failure := s.readSignedBody(w, r)
	if failure != nil {
		s.writeRejection(w, http.StatusUnauthorized, failure)
		return
	}
	query, err := parseDeploymentDescriptorQuery(body)
	if err != nil || r.URL.RawQuery != "" || !query.Legacy.negotiated() || query.DeploymentIDs != nil {
		s.writeRejection(w, http.StatusBadRequest, contract.NewError(
			newLocalRequestID(), contract.CodeInvalidRequest, "explicit scoped catalogue negotiation required"))
		return
	}
	descriptors := s.inventory.Current().DeploymentDescriptors()
	s.attachCommercial(descriptors)
	if err := validateUniqueDeploymentDescriptors(descriptors); err != nil {
		s.writeRejection(w, http.StatusServiceUnavailable, contract.NewError(
			newLocalRequestID(), contract.CodeServiceUnavailable,
			"the deployment identity is ambiguous in the serving snapshot"))
		return
	}
	writeJSON(w, http.StatusOK, legacyModelsQueryResponse{
		modelsResponse:    s.modelsPayload(time.Now()),
		legacyNegotiation: query.Legacy,
		Deployments:       descriptors,
	})
}
