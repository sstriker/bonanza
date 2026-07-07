package starlark

import (
	"errors"
	"fmt"
	"slices"
	"sort"

	pg_label "bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"
	"bonanza.build/pkg/storage/object"

	"go.starlark.net/starlark"
)

// Aspect represents a Starlark aspect object. Aspects allow augmenting
// build dependency graphs with additional information and actions.
type Aspect[TReference any, TMetadata model_core.ReferenceMetadata] struct {
	LateNamedValue
	definition AspectDefinition[TReference, TMetadata]
}

var (
	_ EncodableValue[object.LocalReference, model_core.ReferenceMetadata] = (*Aspect[object.LocalReference, model_core.ReferenceMetadata])(nil)
	_ NamedGlobal                                                         = (*Aspect[object.LocalReference, model_core.ReferenceMetadata])(nil)
)

// NewAspect creates a new Starlark aspect object, which is typically
// performed by calling the aspect() constructor function.
func NewAspect[TReference any, TMetadata model_core.ReferenceMetadata](identifier *pg_label.CanonicalStarlarkIdentifier, definition AspectDefinition[TReference, TMetadata]) *Aspect[TReference, TMetadata] {
	return &Aspect[TReference, TMetadata]{
		LateNamedValue: LateNamedValue{
			Identifier: identifier,
		},
		definition: definition,
	}
}

func (Aspect[TReference, TMetadata]) String() string {
	return "<aspect>"
}

// Type returns the type name of a Starlark aspect object in string
// form.
func (Aspect[TReference, TMetadata]) Type() string {
	return "Aspect"
}

// Freeze a Starlark aspect object, so that it becomes immutable. This
// has no effect, as Starlark aspect objects have no mutable properties.
func (Aspect[TReference, TMetadata]) Freeze() {}

// Truth returns whether a Starlark aspect object is a "truthy" or a
// "falsy". Starlark aspect objects are always "truthy".
func (Aspect[TReference, TMetadata]) Truth() starlark.Bool {
	return starlark.True
}

// Hash a Starlark aspect object, so that it can be placed in a set or
// be used as a key in a dict. However, Starlark aspect objects cannot
// be hashed.
func (Aspect[TReference, TMetadata]) Hash(thread *starlark.Thread) (uint32, error) {
	return 0, errors.New("aspect cannot be hashed")
}

// EncodeValue encodes a Starlark aspect object as a Protobuf message,
// so that it can be written to storage.
func (a *Aspect[TReference, TMetadata]) EncodeValue(path map[starlark.Value]struct{}, currentIdentifier *pg_label.CanonicalStarlarkIdentifier, options *ValueEncodingOptions[TReference, TMetadata]) (model_core.PatchedMessage[*model_starlark_pb.Value, TMetadata], bool, error) {
	if a.Identifier == nil {
		return model_core.PatchedMessage[*model_starlark_pb.Value, TMetadata]{}, false, errors.New("aspect does not have a name")
	}
	if currentIdentifier == nil || *currentIdentifier != *a.Identifier {
		// Not the canonical identifier under which this aspect
		// is known. Emit a reference.
		return model_core.NewSimplePatchedMessage[TMetadata](
			&model_starlark_pb.Value{
				Kind: &model_starlark_pb.Value_Aspect{
					Aspect: &model_starlark_pb.Aspect{
						Kind: &model_starlark_pb.Aspect_Reference{
							Reference: a.Identifier.String(),
						},
					},
				},
			},
		), false, nil
	}

	if a.definition == nil {
		return model_core.PatchedMessage[*model_starlark_pb.Value, TMetadata]{}, false, errors.New("aspect does not have a definition")
	}
	definition, needsCode, err := a.definition.Encode(path, options)
	if err != nil {
		return model_core.PatchedMessage[*model_starlark_pb.Value, TMetadata]{}, false, err
	}
	return model_core.NewPatchedMessage(
		&model_starlark_pb.Value{
			Kind: &model_starlark_pb.Value_Aspect{
				Aspect: &model_starlark_pb.Aspect{
					Kind: &model_starlark_pb.Aspect_Definition_{
						Definition: definition.Message,
					},
				},
			},
		},
		definition.Patcher,
	), needsCode, nil
}

// AspectDefinition contains the definition of an aspect.
type AspectDefinition[TReference any, TMetadata model_core.ReferenceMetadata] interface {
	Encode(path map[starlark.Value]struct{}, options *ValueEncodingOptions[TReference, TMetadata]) (model_core.PatchedMessage[*model_starlark_pb.Aspect_Definition, TMetadata], bool, error)
}

type starlarkAspectDefinition[TReference object.BasicReference, TMetadata model_core.ReferenceMetadata] struct {
	attrAspects             []string
	attrs                   map[pg_label.StarlarkIdentifier]*Attr[TReference, TMetadata]
	implementation          NamedFunction[TReference, TMetadata]
	requiredProviders       [][]*Provider[TReference, TMetadata]
	requiredAspectProviders [][]*Provider[TReference, TMetadata]
	requires                []*Aspect[TReference, TMetadata]
	provides                []*Provider[TReference, TMetadata]
}

// NewStarlarkAspectDefinition creates the definition of an aspect,
// given the parameters that were provided to the aspect() function.
func NewStarlarkAspectDefinition[TReference object.BasicReference, TMetadata model_core.ReferenceMetadata](
	attrAspects []string,
	attrs map[pg_label.StarlarkIdentifier]*Attr[TReference, TMetadata],
	implementation NamedFunction[TReference, TMetadata],
	requiredProviders [][]*Provider[TReference, TMetadata],
	requiredAspectProviders [][]*Provider[TReference, TMetadata],
	requires []*Aspect[TReference, TMetadata],
	provides []*Provider[TReference, TMetadata],
) AspectDefinition[TReference, TMetadata] {
	return &starlarkAspectDefinition[TReference, TMetadata]{
		attrAspects:             attrAspects,
		attrs:                   attrs,
		implementation:          implementation,
		requiredProviders:       requiredProviders,
		requiredAspectProviders: requiredAspectProviders,
		requires:                requires,
		provides:                provides,
	}
}

func (ad *starlarkAspectDefinition[TReference, TMetadata]) Encode(path map[starlark.Value]struct{}, options *ValueEncodingOptions[TReference, TMetadata]) (model_core.PatchedMessage[*model_starlark_pb.Aspect_Definition, TMetadata], bool, error) {
	implementation, needsCode, err := ad.implementation.Encode(path, options)
	if err != nil {
		return model_core.PatchedMessage[*model_starlark_pb.Aspect_Definition, TMetadata]{}, false, err
	}

	attrAspects := slices.Clone(ad.attrAspects)
	sort.Strings(attrAspects)

	namedAttrs, namedAttrsNeedCode, err := encodeNamedAttrs(ad.attrs, path, options)
	if err != nil {
		return model_core.PatchedMessage[*model_starlark_pb.Aspect_Definition, TMetadata]{}, false, err
	}
	needsCode = needsCode || namedAttrsNeedCode

	requiredProviders, err := encodeRequiredProviderSets(ad.requiredProviders)
	if err != nil {
		return model_core.PatchedMessage[*model_starlark_pb.Aspect_Definition, TMetadata]{}, false, fmt.Errorf("required_providers: %w", err)
	}
	requiredAspectProviders, err := encodeRequiredProviderSets(ad.requiredAspectProviders)
	if err != nil {
		return model_core.PatchedMessage[*model_starlark_pb.Aspect_Definition, TMetadata]{}, false, fmt.Errorf("required_aspect_providers: %w", err)
	}
	requires, err := aspectIdentifierStrings(ad.requires)
	if err != nil {
		return model_core.PatchedMessage[*model_starlark_pb.Aspect_Definition, TMetadata]{}, false, fmt.Errorf("requires: %w", err)
	}
	provides, err := providerIdentifierStrings[TReference, TMetadata](ad.provides)
	if err != nil {
		return model_core.PatchedMessage[*model_starlark_pb.Aspect_Definition, TMetadata]{}, false, fmt.Errorf("provides: %w", err)
	}

	patcher := implementation.Patcher
	patcher.Merge(namedAttrs.Patcher)
	return model_core.NewPatchedMessage(
		&model_starlark_pb.Aspect_Definition{
			AttrAspects:             slices.Compact(attrAspects),
			Attrs:                   namedAttrs.Message,
			Implementation:          implementation.Message,
			RequiredProviders:       requiredProviders,
			RequiredAspectProviders: requiredAspectProviders,
			Requires:                requires,
			Provides:                provides,
		},
		patcher,
	), needsCode, nil
}

type protoAspectDefinition[TReference any, TMetadata model_core.ReferenceMetadata] struct{}

// NewProtoAspectDefinition contains the definition of an aspect that
// was declared in another .bzl file and has subsequently been written
// to storage.
//
// As aspects are only accessed during target configuration and this is
// not done by directly referencing the Starlark value object, there is
// no need for this type to retain any information. There is also no way
// for the definition of an aspect to be carried over between .bzl
// files, as such indirection is always done by referencing the original
// identifier of the aspect. This type therefore merely acts as a
// placeholder.
func NewProtoAspectDefinition[TReference any, TMetadata model_core.ReferenceMetadata]() AspectDefinition[TReference, TMetadata] {
	return &protoAspectDefinition[TReference, TMetadata]{}
}

func (protoAspectDefinition[TReference, TMetadata]) Encode(path map[starlark.Value]struct{}, options *ValueEncodingOptions[TReference, TMetadata]) (model_core.PatchedMessage[*model_starlark_pb.Aspect_Definition, TMetadata], bool, error) {
	panic("aspect definition was already encoded previously")
}
