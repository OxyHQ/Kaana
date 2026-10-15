package contract

import "fmt"

// AvailabilityScope is who a route may be served to (`availabilityScopeSchema`).
type AvailabilityScope string

const (
	ScopePlatformInternal AvailabilityScope = "platform_internal"
	ScopePublicPAYG       AvailabilityScope = "public_payg"
	ScopeEnterprise       AvailabilityScope = "enterprise"
	ScopeBYOKOnly         AvailabilityScope = "byok_only"
	ScopeOxyHosted        AvailabilityScope = "oxy_hosted"
)

var availabilityScopeValues = []AvailabilityScope{
	ScopePlatformInternal, ScopePublicPAYG, ScopeEnterprise, ScopeBYOKOnly, ScopeOxyHosted,
}

// Valid reports whether the scope is one the contract names.
func (s AvailabilityScope) Valid() bool { return isMember(s, availabilityScopeValues) }

// CommercialPermission is the basis on which a route may be served
// (`commercialPermissionSchema`).
type CommercialPermission string

const (
	PermissionStandardApplicationUse CommercialPermission = "standard_application_use"
	PermissionPublicResaleApproved   CommercialPermission = "public_resale_approved"
	PermissionWholesaleContract      CommercialPermission = "wholesale_contract"
	PermissionCustomerBYOK           CommercialPermission = "customer_byok"
	PermissionOpenWeightHosting      CommercialPermission = "open_weight_hosting"
)

var commercialPermissionValues = []CommercialPermission{
	PermissionStandardApplicationUse, PermissionPublicResaleApproved,
	PermissionWholesaleContract, PermissionCustomerBYOK, PermissionOpenWeightHosting,
}

// Valid reports whether the permission is one the contract names.
func (p CommercialPermission) Valid() bool { return isMember(p, commercialPermissionValues) }

// DeploymentCommercial is the `commercial` member of a deployment descriptor
// (`deploymentCommercialSchema`, contract set 4.4.0). A deployment that is not
// sold has none.
type DeploymentCommercial struct {
	AvailabilityScope    AvailabilityScope    `json:"availabilityScope"`
	CommercialPermission CommercialPermission `json:"commercialPermission"`
	// DedicatedCapacity is reserved capacity for one enterprise account.
	// Absent means shared (false).
	DedicatedCapacity *bool `json:"dedicatedCapacity,omitempty"`
}

// Validate applies the contract's refinements: both members are named values,
// `customer_byok` is the permission of `byok_only` and of no other scope, and
// a public pay-as-you-go route carries an approved resale permission.
func (c DeploymentCommercial) Validate() error {
	if !c.AvailabilityScope.Valid() {
		return fmt.Errorf("unknown availabilityScope %q", c.AvailabilityScope)
	}
	if !c.CommercialPermission.Valid() {
		return fmt.Errorf("unknown commercialPermission %q", c.CommercialPermission)
	}
	byokScope := c.AvailabilityScope == ScopeBYOKOnly
	byokPermission := c.CommercialPermission == PermissionCustomerBYOK
	if byokScope != byokPermission {
		return fmt.Errorf("customer_byok is the permission of a byok_only route, and only of it (got %s with %s)", c.CommercialPermission, c.AvailabilityScope)
	}
	if c.AvailabilityScope == ScopePublicPAYG &&
		c.CommercialPermission != PermissionPublicResaleApproved &&
		c.CommercialPermission != PermissionWholesaleContract &&
		c.CommercialPermission != PermissionOpenWeightHosting {
		return fmt.Errorf("a public_payg route requires an approved resale permission, got %s", c.CommercialPermission)
	}
	return nil
}
