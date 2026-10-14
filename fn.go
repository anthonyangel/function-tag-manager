package main

import (
	"context"
	"strings"

	"github.com/crossplane-contrib/function-tag-manager/filters"
	"github.com/crossplane-contrib/function-tag-manager/input/v1beta1"
	fncontext "github.com/crossplane/function-sdk-go/context"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/request"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/response"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/fieldpath"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

// Function manages tags on composed resources based on configuration rules.
type Function struct {
	fnv1.FunctionRunnerServiceServer

	log logging.Logger
}

// RunFunction runs the Function.
func (f *Function) RunFunction(_ context.Context, req *fnv1.RunFunctionRequest) (*fnv1.RunFunctionResponse, error) {
	f.log.Info("Running Function", "tag-manager", req.GetMeta().GetTag())

	rsp := response.To(req, response.DefaultTTL)

	in := &v1beta1.ManagedTags{}

	err := request.GetInput(req, in)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get Function input from %T", req))
		return rsp, nil
	}

	oxr, err := request.GetObservedCompositeResource(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get observed composite resource from %T", req))
		return rsp, nil
	}

	env := &unstructured.Unstructured{}
	if v, ok := request.GetContextKey(req, fncontext.KeyEnvironment); ok {
		err := resource.AsObject(v.GetStructValue(), env)
		if err != nil {
			response.Fatal(rsp, errors.Wrapf(err, "cannot get Composition environment from %T context key %q", req, fncontext.KeyEnvironment))
			return rsp, nil
		}

		f.log.Debug("Loaded Composition environment from Function context", "context-key", fncontext.KeyEnvironment)
	}

	f.log.WithValues(
		"xr-d", oxr.Resource.GetAPIVersion(),
		"xr-kind", oxr.Resource.GetKind(),
		"xr-name", oxr.Resource.GetName(),
	)

	// Process all the AddTags into 2 groups based on Policy: Replace or Retain
	// we also need to resolve any tags coming from a Composite fieldpath
	additionalTags := f.ResolveAddTags(in.AddTags, oxr, env)

	// The composed resources that actually exist.
	observedComposed, err := request.GetObservedComposedResources(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get observed composed resources from %T", req))
		return rsp, nil
	}

	// The composed resources desired by any previous Functions in the pipeline.
	desiredComposed, err := request.GetDesiredComposedResources(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get desired composed resources from %T", req))
		return rsp, nil
	}

	// Resolve tags to remove once for all resources (doesn't depend on individual resources)
	removeTags := f.ResolveRemoveTags(in.RemoveTags, oxr, env)

	// Decide what to do to every resource, and check that the provider will
	// accept the tags, before any resource is changed.
	plans, err := f.PlanTags(in, oxr, env, desiredComposed, observedComposed, additionalTags, removeTags)
	if err != nil {
		response.Fatal(rsp, err)
		return rsp, nil
	}

	for name, p := range plans {
		desired := desiredComposed[name]

		err := MergeTags(desired, additionalTags, p.field)
		if err != nil {
			f.log.Debug("error adding tags", "resource", string(name), "error", err.Error())
		}

		if p.ignoreTags != nil {
			err := MergeTags(desired, *p.ignoreTags, p.field)
			if err != nil {
				f.log.Debug("error adding tags to ignore", "resource", string(name), "error", err.Error())
			}
		}

		// Remove tags
		if len(removeTags) > 0 {
			err := RemoveTags(desired, removeTags, p.field)
			if err != nil {
				f.log.Debug("error removing tags", "resource", string(name), "error", err.Error())
			}
		}
	}

	err = response.SetDesiredComposedResources(rsp, desiredComposed)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot set desired composed resources in %T", rsp))
		return rsp, nil
	}

	response.Normalf(rsp, "Successfully Processed tags")

	return rsp, nil
}

// TagPlan is what the Function will do to a desired composed resource.
type TagPlan struct {
	// field is the spec.forProvider field that holds the resource's tags.
	field string
	// ignoreTags are tags to take from the observed resource.
	ignoreTags *TagUpdater
}

// PlanTags decides which of the desired composed resources the Function will
// change, and returns an error if the tags it would add aren't valid for the
// resource's provider.
func (f *Function) PlanTags(in *v1beta1.ManagedTags, oxr *resource.Composite, env *unstructured.Unstructured, desiredComposed map[resource.Name]*resource.DesiredComposed, observedComposed map[resource.Name]resource.ObservedComposed, additionalTags TagUpdater, removeTags []string) (map[resource.Name]TagPlan, error) {
	resourceFilter := filters.NewResourceFilter()
	resourceRules := filters.NewResourceRules()
	plans := make(map[resource.Name]TagPlan, len(desiredComposed))

	for name, desired := range desiredComposed {
		if IgnoreResource(desired) {
			f.log.Debug("skipping resource due to ignore annotation or label", "resource", string(name), "gvk", desired.Resource.GroupVersionKind().String())
			continue
		}

		field, ok := ManagedField(desired, resourceFilter)
		if !ok {
			f.log.Debug("skipping resource that doesn't support tags", "resource", string(name), "gvk", desired.Resource.GroupVersionKind().String())
			continue
		}

		p := TagPlan{field: field}

		// Ignore tags only if there is an existing Composed resource with tags in the status
		if observed, ok := observedComposed[name]; ok {
			p.ignoreTags = f.ResolveIgnoreTags(in.IgnoreTags, oxr, &observed, env, field)
		}

		rules, ok := ManagedRules(desired, resourceRules)
		if ok {
			adding := []v1beta1.Tags{additionalTags.Retain, additionalTags.Replace}
			if p.ignoreTags != nil {
				adding = append(adding, p.ignoreTags.Retain, p.ignoreTags.Replace)
			}

			err := ValidateTags(desired, rules, field, adding, removeTags)
			if err != nil {
				return nil, errors.Wrapf(err, "invalid tags for composed resource %q (%s)", string(name), groupKind(desired))
			}
		}

		plans[name] = p
	}

	return plans, nil
}

// IgnoreResource whether this resource has a label or annotation set to ignore.
// If the annotation is present, it takes precedence over the label.
func IgnoreResource(dc *resource.DesiredComposed) bool {
	if dc == nil {
		return true
	}

	annotations := dc.Resource.GetAnnotations()

	// Check annotation first - if present, it takes precedence
	aval, ok := annotations[IgnoreResourceAnnotation]
	if ok {
		return strings.EqualFold(aval, "true")
	}

	// Fall back to label for backward compatibility
	var labels map[string]any

	err := fieldpath.Pave(dc.Resource.Object).GetValueInto("metadata.labels", &labels)
	if err != nil {
		return false
	}

	val, ok := labels[IgnoreResourceLabel].(string)
	if ok && strings.EqualFold(val, "true") {
		return true
	}

	return false
}
