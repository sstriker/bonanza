package starlark_test

import (
	"bytes"
	"fmt"
	"testing"

	"bonanza.build/pkg/label"
	model_core "bonanza.build/pkg/model/core"
	model_encoding "bonanza.build/pkg/model/encoding"
	model_starlark "bonanza.build/pkg/model/starlark"
	model_starlark_pb "bonanza.build/pkg/proto/model/starlark"
	object_pb "bonanza.build/pkg/proto/storage/object"
	"bonanza.build/pkg/storage/object"

	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/stretchr/testify/require"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// compileAndEncodeProgram compiles the provided Starlark source code as
// if it were a .bzl file with the provided label and encodes its
// globals to a CompiledProgram message.
func compileAndEncodeProgram(t *testing.T, canonicalLabel label.CanonicalLabel, sourceCode string, bzlFileBuiltins starlark.StringDict) (*starlark.Program, model_core.PatchedMessage[*model_starlark_pb.CompiledProgram, model_core.NoopReferenceMetadata], error) {
	_, program, err := starlark.SourceProgramOptions(
		&syntax.FileOptions{Set: true},
		canonicalLabel.String(),
		sourceCode,
		bzlFileBuiltins.Has,
	)
	require.NoError(t, err)

	globals, err := program.Init(&starlark.Thread{}, bzlFileBuiltins)
	require.NoError(t, err)
	model_starlark.NameAndExtractGlobals(globals, canonicalLabel)

	compiledProgram, err := model_starlark.EncodeCompiledProgram(
		program,
		globals,
		&model_starlark.ValueEncodingOptions[object.LocalReference, model_core.NoopReferenceMetadata]{
			CurrentFilename:        &canonicalLabel,
			Context:                t.Context(),
			ObjectEncoder:          model_encoding.NewChainedDeterministicBinaryEncoder(nil),
			ObjectReferenceFormat:  util.Must(object.NewReferenceFormat(object_pb.ReferenceFormat_SHA256_V1)),
			ObjectCapturer:         model_core.NewDiscardingObjectCapturer[object.LocalReference](),
			ObjectMinimumSizeBytes: 32 * 1024,
			ObjectMaximumSizeBytes: 128 * 1024,
		},
	)
	return program, compiledProgram, err
}

func TestEncodeCompiledProgramRecursiveGlobals(t *testing.T) {
	// Provide a struct() builtin that behaves identically to the
	// one that is normally exposed to .bzl files.
	bzlFileBuiltins := starlark.StringDict{
		"struct": starlark.NewBuiltin(
			"struct",
			func(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				entries := make(map[string]any, len(kwargs))
				for _, kwarg := range kwargs {
					entries[string(kwarg[0].(starlark.String))] = kwarg[1]
				}
				return model_starlark.NewStructFromDict[object.LocalReference, model_core.NoopReferenceMetadata](nil, entries), nil
			},
		),
	}
	canonicalLabel := util.Must(label.NewCanonicalLabel("@@example+//:flags.bzl"))

	// decodeAndCall decodes the globals of an encoded compiled
	// program and returns a Starlark thread that is capable of
	// lazily reloading functions contained in them, mirroring the
	// way in which the analysis code sets up its threads.
	decodeAndCall := func(t *testing.T, program *starlark.Program, compiledProgram model_core.PatchedMessage[*model_starlark_pb.CompiledProgram, model_core.NoopReferenceMetadata]) (starlark.StringDict, *starlark.Thread) {
		var decodedGlobals starlark.StringDict
		valueDecodingOptions := &model_starlark.ValueDecodingOptions[object.LocalReference]{
			Context: t.Context(),
			Readers: &model_starlark.ValueReaders[object.LocalReference]{},
			LabelCreator: func(resolvedLabel label.ResolvedLabel) (starlark.Value, error) {
				return model_starlark.NewLabel[object.LocalReference, model_core.NoopReferenceMetadata](resolvedLabel), nil
			},
			BzlFileBuiltins: bzlFileBuiltins,
			GlobalResolver: func(identifier label.CanonicalStarlarkIdentifier) (starlark.Value, error) {
				// Resolve references against the memoized
				// decoded globals of the file, just like
				// GetCompiledBzlFileDecodedGlobalsValue.
				require.Equal(t, canonicalLabel, identifier.GetCanonicalLabel())
				value, ok := decodedGlobals[identifier.GetStarlarkIdentifier().String()]
				if !ok {
					return nil, fmt.Errorf("global %#v does not exist", identifier.String())
				}
				return value, nil
			},
		}

		var err error
		decodedGlobals, err = model_starlark.DecodeGlobals[object.LocalReference, model_core.NoopReferenceMetadata](
			model_core.NewSimpleMessage[object.LocalReference](compiledProgram.Message.GetGlobals()),
			canonicalLabel,
			valueDecodingOptions,
		)
		require.NoError(t, err)

		// Functions are reloaded from the code that is part of
		// the CompiledProgram message.
		reloadedProgram, err := starlark.CompiledProgram(bytes.NewBuffer(compiledProgram.Message.Code))
		require.NoError(t, err)
		functionFactory, factoryGlobals, err := reloadedProgram.NewFunctionFactory(&starlark.Thread{}, bzlFileBuiltins)
		require.NoError(t, err)
		model_starlark.NameAndExtractGlobals(factoryGlobals, canonicalLabel)
		factoryGlobals.Freeze()

		thread := &starlark.Thread{}
		thread.SetLocal(model_starlark.ValueDecodingOptionsKey, valueDecodingOptions)
		thread.SetLocal(model_starlark.FunctionFactoryResolverKey, model_starlark.FunctionFactoryResolver(func(filename label.CanonicalLabel) (*starlark.FunctionFactory, error) {
			require.Equal(t, canonicalLabel, filename)
			return functionFactory, nil
		}))
		return decodedGlobals, thread
	}

	t.Run("SelfCapturingStruct", func(t *testing.T) {
		// Struct global whose method closures capture the
		// struct itself through a closure cell, as done by
		// rules_python's enum-like structs. The resulting value
		// is defined recursively, but must still be encodable,
		// as the cycle re-enters a persisted global at a lazily
		// decoded closure position.
		program, compiledProgram, err := compileAndEncodeProgram(t, canonicalLabel, `
def _create_flag_enum(value):
    def get_self():
        return self

    self = struct(
        get_self = get_self,
        get_value = lambda: self.value,
        value = value,
    )
    return self

MY_FLAG = _create_flag_enum("hello")
`, bzlFileBuiltins)
		require.NoError(t, err)

		// The stored object graph must be acyclic: the "self"
		// free variable of get_self() must have been encoded as
		// a reference to the MY_FLAG global.
		encodedGlobals := compiledProgram.Message.GetGlobals()
		require.Equal(t, []string{"MY_FLAG"}, encodedGlobals.Keys)
		require.Len(t, encodedGlobals.Values, 1)
		encodedStruct := encodedGlobals.Values[0].GetLeaf().GetStruct()
		require.NotNil(t, encodedStruct)
		require.Equal(t, []string{"get_self", "get_value", "value"}, encodedStruct.Fields.Keys)
		closure := encodedStruct.Fields.Values[0].GetLeaf().GetFunction().GetClosure()
		require.NotNil(t, closure)
		require.Len(t, closure.FreeVariables, 1)
		require.Equal(t, "@@example+//:flags.bzl%MY_FLAG", closure.FreeVariables[0].GetGlobalReference())

		// Calling methods on the decoded struct must preserve
		// the identity of the struct within the evaluation.
		decodedGlobals, thread := decodeAndCall(t, program, compiledProgram)
		myFlag, ok := decodedGlobals["MY_FLAG"]
		require.True(t, ok)

		getSelf, err := myFlag.(starlark.HasAttrs).Attr(thread, "get_self")
		require.NoError(t, err)
		self, err := starlark.Call(thread, getSelf, nil, nil)
		require.NoError(t, err)
		require.Same(t, myFlag, self)

		getValue, err := myFlag.(starlark.HasAttrs).Attr(thread, "get_value")
		require.NoError(t, err)
		value, err := starlark.Call(thread, getValue, nil, nil)
		require.NoError(t, err)
		require.Equal(t, starlark.String("hello"), value)
	})

	t.Run("MutuallyRecursiveStructs", func(t *testing.T) {
		// Cycles that span multiple named globals must be
		// broken as well.
		program, compiledProgram, err := compileAndEncodeProgram(t, canonicalLabel, `
def _create_pair():
    a = struct(get_other = lambda: b, name = "a")
    b = struct(get_other = lambda: a, name = "b")
    return a, b

PAIR_A, PAIR_B = _create_pair()
`, bzlFileBuiltins)
		require.NoError(t, err)

		decodedGlobals, thread := decodeAndCall(t, program, compiledProgram)
		pairA, pairB := decodedGlobals["PAIR_A"], decodedGlobals["PAIR_B"]

		getOther, err := pairA.(starlark.HasAttrs).Attr(thread, "get_other")
		require.NoError(t, err)
		other, err := starlark.Call(thread, getOther, nil, nil)
		require.NoError(t, err)
		require.Same(t, pairB, other)

		getOther, err = other.(starlark.HasAttrs).Attr(thread, "get_other")
		require.NoError(t, err)
		other, err = starlark.Call(thread, getOther, nil, nil)
		require.NoError(t, err)
		require.Same(t, pairA, other)
	})

	t.Run("AliasedGlobals", func(t *testing.T) {
		// If the same value is bound to multiple globals, each
		// global is persisted as an independent copy. The
		// self-references inside each copy must resolve back to
		// the copy itself, so that methods returning the value
		// preserve the identity of the global through which
		// they were reached.
		program, compiledProgram, err := compileAndEncodeProgram(t, canonicalLabel, `
def _create_flag_enum():
    self = struct(get_self = lambda: self)
    return self

FLAG_B = _create_flag_enum()
FLAG_A = FLAG_B
`, bzlFileBuiltins)
		require.NoError(t, err)

		encodedGlobals := compiledProgram.Message.GetGlobals()
		require.Equal(t, []string{"FLAG_A", "FLAG_B"}, encodedGlobals.Keys)
		for i, name := range encodedGlobals.Keys {
			encodedStruct := encodedGlobals.Values[i].GetLeaf().GetStruct()
			require.NotNil(t, encodedStruct)
			closure := encodedStruct.Fields.Values[0].GetLeaf().GetFunction().GetClosure()
			require.NotNil(t, closure)
			require.Len(t, closure.FreeVariables, 1)
			require.Equal(t, "@@example+//:flags.bzl%"+name, closure.FreeVariables[0].GetGlobalReference())
		}

		decodedGlobals, thread := decodeAndCall(t, program, compiledProgram)
		for _, name := range encodedGlobals.Keys {
			flag := decodedGlobals[name]
			getSelf, err := flag.(starlark.HasAttrs).Attr(thread, "get_self")
			require.NoError(t, err)
			self, err := starlark.Call(thread, getSelf, nil, nil)
			require.NoError(t, err)
			require.Same(t, flag, self)
		}
	})

	t.Run("GlobalsBeneathClosuresAreInlined", func(t *testing.T) {
		// Globals that are merely contained in values captured
		// by a closure must still be encoded inline. Emitting
		// global references at such nested positions would
		// cause them to leak into eagerly decoded positions
		// when the containing value is re-encoded into another
		// file verbatim (e.g. as a struct field).
		_, compiledProgram, err := compileAndEncodeProgram(t, canonicalLabel, `
FLAG = struct(name = "f")

def _make():
    config = struct(flag = FLAG)
    return lambda: config

get_config = _make()
`, bzlFileBuiltins)
		require.NoError(t, err)

		encodedGlobals := compiledProgram.Message.GetGlobals()
		require.Equal(t, []string{"FLAG", "get_config"}, encodedGlobals.Keys)
		closure := encodedGlobals.Values[1].GetLeaf().GetFunction().GetClosure()
		require.NotNil(t, closure)
		require.Len(t, closure.FreeVariables, 1)
		configStruct := closure.FreeVariables[0].GetStruct()
		require.NotNil(t, configStruct)
		require.Equal(t, []string{"flag"}, configStruct.Fields.Keys)
		flagStruct := configStruct.Fields.Values[0].GetLeaf().GetStruct()
		require.NotNil(t, flagStruct)
		require.Equal(t, []string{"name"}, flagStruct.Fields.Keys)
	})

	t.Run("SelfReferentialListStillFails", func(t *testing.T) {
		// Cycles that do not pass through a function closure
		// would end up at eagerly decoded positions, which
		// would cause decoding of the file's own globals to
		// cycle. These must still be reported as errors.
		_, _, err := compileAndEncodeProgram(t, canonicalLabel, `
MY_LIST = []
MY_LIST.append(MY_LIST)
`, bzlFileBuiltins)
		require.ErrorContains(t, err, "global \"MY_LIST\": value is defined recursively")
	})
}
