package publisher

import (
	"encoding/json"
	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/inventory"
	"github.com/OxyHQ/Kaana/internal/scopedpermit"
	"os"
	"testing"
	"time"
)

func autoPublicationFixture(t *testing.T) ([]Discovery, *privateAutoPublicationPermit, time.Time) {
	discoveries, legacy := privateFixture(t)
	at := legacy.Eligibility.now
	raw, err := os.ReadFile("../contract/testdata/private-auto/golden-approval.json")
	if err != nil {
		t.Fatal(err)
	}
	var source contract.PrivateAutoSourceApproval
	if err = json.Unmarshal(raw, &source); err != nil {
		t.Fatal(err)
	}
	source.DeploymentID = legacy.Audience.DeploymentID
	source.KeyID = legacy.Audience.KeyID
	source.ModelReference = legacy.Audience.ModelReference
	source.UpstreamModelID = legacy.Audience.UpstreamModelID
	source.ProviderRateCardVersionID = legacy.Audience.ProviderRateCardVersionID
	source.ProviderSourceVersion = legacy.Audience.ProviderSourceVersion
	permit, err := privateAutoPermitForApproval(&source, legacy.Cards, legacy.Eligibility, at)
	if err != nil || permit == nil {
		t.Fatal("fixture source unavailable", err)
	}
	return discoveries, permit, at
}
func TestPrivateAutoPublicationRequiresFreshPrivateReviewAndDiscovery(t *testing.T) {
	discoveries, permit, at := autoPublicationFixture(t)
	allow := func(contract.DeploymentID, contract.ProviderSlug) (Withholding, bool) { return Withholding{}, false }
	if source := scopedpermit.SourceReviewedPrivateAutoApproval(); source == nil || source.Validate() != nil {
		t.Fatal("reviewed production Auto source missing")
	}
	candidate, err := privateAutoCandidate(discoveries, permit, at, allow, false)
	if err != nil || candidate == nil || candidate.ScopedExecution != nil || candidate.Current || candidate.PrivateAutoSourceApproval == nil {
		t.Fatal("private publication failed", err)
	}
	raw, _ := json.Marshal(map[string]any{"snapshotId": "auto", "issuedAt": contract.NewTimestamp(at), "deployments": []snapshotDeployment{*candidate}})
	inv, err := inventory.Parse(raw, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Catalogue()) != 0 || len(inv.CatalogueScoped(at)) != 0 || len(inv.DeploymentDescriptors()) != 0 || len(inv.DeploymentDescriptorsScopedAt(at)) != 0 || len(inv.PublicDeployments()) != 0 {
		t.Fatal("private Auto appeared ordinary or commissioning")
	}
	if _, err = inv.Resolve(candidate.ModelReference, at); err == nil {
		t.Fatal("public route admitted")
	}
	set, err := inv.ResolvePrivateAuto(candidate.ModelReference, at, &permit.Approval)
	if err != nil || set.Len() != 1 {
		t.Fatal("private source lost", err)
	}
	if len(inv.DeploymentDescriptorsPrivateAutoAt(at, &permit.Approval)) != 1 || len(inv.CataloguePrivateAuto(at, &permit.Approval)) != 1 {
		t.Fatal("negotiated source projection missing")
	}
	changed := permit.Approval
	changed.ApprovalVersion++
	if _, err = inv.ResolvePrivateAuto(candidate.ModelReference, at, &changed); err == nil || len(inv.DeploymentDescriptorsPrivateAutoAt(at, &changed)) != 0 {
		t.Fatal("foreign source approval admitted")
	}
	for name, change := range map[string]func([]Discovery, *privateAutoPublicationPermit){
		"auth": func(d []Discovery, p *privateAutoPublicationPermit) { d[0].Provider.APIKey = "" },
		"key":  func(d []Discovery, p *privateAutoPublicationPermit) { d[0].Provider.CredentialKeyID = "foreign" },
		"fresh": func(d []Discovery, p *privateAutoPublicationPermit) {
			p.Eligibility.now = p.Eligibility.now.Add(-time.Second)
		},
		"price": func(d []Discovery, p *privateAutoPublicationPermit) {
			d[0].Models[0].Observed.ListPrice.Input = "0.043"
		},
		"privatelegal": func(d []Discovery, p *privateAutoPublicationPermit) { p.Approval.Review.InternalUseAllowed = false },
		"privacy":      func(d []Discovery, p *privateAutoPublicationPermit) { p.Approval.Review.RetainsPayloads = true },
		"expiry": func(d []Discovery, p *privateAutoPublicationPermit) {
			p.Approval.Review.EvidenceExpiresAt = at.Format(time.RFC3339)
		},
		"region": func(d []Discovery, p *privateAutoPublicationPermit) {
			p.Approval.Regions = []contract.Region{"foreign-region"}
		},
		"unservable": func(d []Discovery, p *privateAutoPublicationPermit) { d[0].Models[0].Unservable = "noteligible" },
	} {
		t.Run(name, func(t *testing.T) {
			d, p, at := autoPublicationFixture(t)
			change(d, p)
			got, err := privateAutoCandidate(d, p, at, allow, false)
			if err == nil || got != nil {
				t.Fatal("missing prerequisite published")
			}
		})
	}
	if got, err := privateAutoCandidate(discoveries, permit, at, allow, true); err == nil || got != nil {
		t.Fatal("report-only published private")
	}
}

func TestPrivateAutoOmissionPreservesOrdinaryAndNeverCarriesStalePrivateRow(t *testing.T) {
	discoveries, permit, at := autoPublicationFixture(t)
	ordinary := Discovery{Provider: Provider{Slug: "groq"}, Models: []DiscoveredModel{{UpstreamModelID: "gpt-oss-120b"}}}
	discoveries = append(discoveries, ordinary)
	allow := func(contract.DeploymentID, contract.ProviderSlug) (Withholding, bool) { return Withholding{}, false }
	previous, err := buildSnapshotWithholding(discoveries, testAttribution(t), Observations{}, at, allow, false, nil, permit)
	if err != nil || previous.Deployments != 2 {
		t.Fatal("positive snapshot", err)
	}
	observations, err := ObservationsFrom(previous.Body)
	if err != nil {
		t.Fatal(err)
	}
	var ordinaryReference contract.ModelReference
	for _, d := range parseSnapshot(t, previous.Body).Deployments {
		if d.Provider == "groq" {
			ordinaryReference = d.ModelReference
		}
	}
	permit.Eligibility.now = at.Add(time.Second)
	permit.Approval.Review.InternalUseAllowed = false
	next, err := buildSnapshotWithholding(discoveries, testAttribution(t), observations, at.Add(time.Second), allow, false, nil, permit)
	if err != nil {
		t.Fatal("private proof failure stopped ordinary refresh", err)
	}
	snapshot := parseSnapshot(t, next.Body)
	if len(snapshot.Deployments) != 1 || snapshot.Deployments[0].Provider != "groq" || !next.PrivatePublicationOmitted {
		t.Fatal("stale private carried or ordinary lost")
	}
	if snapshot.Deployments[0].ModelReference != ordinaryReference {
		t.Fatal("ordinary observation rejuvenated")
	}
	discoveries, auto, at := autoPublicationFixture(t)
	_, commissioning := privateFixture(t)
	discoveries = append(discoveries, ordinary)
	conflict, err := buildSnapshotWithholding(discoveries, testAttribution(t), Observations{}, at, allow, false, commissioning, auto)
	if err != nil {
		t.Fatal("same-deployment conflict stopped ordinary refresh", err)
	}
	for _, d := range parseSnapshot(t, conflict.Body).Deployments {
		if d.PrivateAutoSourceApproval != nil {
			t.Fatal("3.6/3.7 both published same deployment")
		}
	}
	if !conflict.PrivatePublicationOmitted {
		t.Fatal("conflict not diagnosed")
	}
}

func TestPrivateAutoSnapshotHashBindsApprovalAndObservedPrice(t *testing.T) {
	d, p, at := autoPublicationFixture(t)
	candidate, err := privateAutoCandidate(d, p, at, func(contract.DeploymentID, contract.ProviderSlug) (Withholding, bool) { return Withholding{}, false }, false)
	if err != nil {
		t.Fatal(err)
	}
	original := contentID([]snapshotDeployment{*candidate})
	candidate.PrivateAutoSourceApproval.ApprovalVersion++
	if contentID([]snapshotDeployment{*candidate}) == original {
		t.Fatal("approval version absent from snapshot ID")
	}
	candidate.PrivateAutoSourceApproval.ApprovalVersion--
	candidate.Observed.ListPrice.Input = "0.043"
	if contentID([]snapshotDeployment{*candidate}) == original {
		t.Fatal("private tariff absent from snapshot ID")
	}
}
