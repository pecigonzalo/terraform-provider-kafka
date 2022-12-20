package modifier

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func StringDefaultValue(v types.String) planmodifier.String {
	return &stringDefaultValuePlanModifier{v}
}

// https://github.com/hashicorp/terraform-plugin-framework/issues/285
type stringDefaultValuePlanModifier struct {
	DefaultValue types.String
}

var _ planmodifier.String = (*stringDefaultValuePlanModifier)(nil)

func (apm *stringDefaultValuePlanModifier) PlanModifyString(ctx context.Context, req planmodifier.StringRequest, res *planmodifier.StringResponse) {
	// If the attribute configuration is not null, we are done here
	if !req.ConfigValue.IsNull() {
		return
	}
	// If the attribute plan is "known" and "not null", then a previous plan modifier in the sequence
	// has already been applied, and we don't want to interfere.
	if !req.PlanValue.IsUnknown() && !req.PlanValue.IsNull() {
		return
	}
	res.PlanValue = apm.DefaultValue
}

func (apm stringDefaultValuePlanModifier) Description(ctx context.Context) string {
	return "Use a static default value for an attribute"
}

func (apm stringDefaultValuePlanModifier) MarkdownDescription(ctx context.Context) string {
	return apm.Description(ctx)
}

func MapDefaultValue(v types.Map) planmodifier.Map {
	return &mapDefaultValuePlanModifier{v}
}

// https://github.com/hashicorp/terraform-plugin-framework/issues/285
type mapDefaultValuePlanModifier struct {
	DefaultValue types.Map
}

var _ planmodifier.Map = (*mapDefaultValuePlanModifier)(nil)

func (apm *mapDefaultValuePlanModifier) PlanModifyMap(ctx context.Context, req planmodifier.MapRequest, res *planmodifier.MapResponse) {
	// If the attribute configuration is not null, we are done here
	if !req.ConfigValue.IsNull() {
		return
	}
	// If the attribute plan is "known" and "not null", then a previous plan modifier in the sequence
	// has already been applied, and we don't want to interfere.
	if !req.PlanValue.IsUnknown() && !req.PlanValue.IsNull() {
		return
	}
	res.PlanValue = apm.DefaultValue
}

func (apm mapDefaultValuePlanModifier) Description(ctx context.Context) string {
	return "Use a static default value for an attribute"
}

func (apm mapDefaultValuePlanModifier) MarkdownDescription(ctx context.Context) string {
	return apm.Description(ctx)
}
