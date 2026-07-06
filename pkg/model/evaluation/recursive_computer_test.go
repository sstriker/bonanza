package evaluation_test

import (
	"context"
	"errors"
	"testing"

	model_core "bonanza.build/pkg/model/core"
	model_encoding "bonanza.build/pkg/model/encoding"
	model_evaluation "bonanza.build/pkg/model/evaluation"
	model_core_pb "bonanza.build/pkg/proto/model/core"
	model_evaluation_pb "bonanza.build/pkg/proto/model/evaluation"
	model_evaluation_cache_pb "bonanza.build/pkg/proto/model/evaluation/cache"
	"bonanza.build/pkg/storage/object"

	"github.com/buildbarn/bb-storage/pkg/clock"
	"github.com/buildbarn/bb-storage/pkg/program"
	"github.com/buildbarn/bb-storage/pkg/testutil"
	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/stretchr/testify/require"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"go.uber.org/mock/gomock"
)

func TestRecursiveComputer(t *testing.T) {
	ctrl, ctx := gomock.WithContext(context.Background(), t)

	t.Run("Fibonacci", func(t *testing.T) {
		// Example usage, where we provide a very basic
		// implementation of Computer that attempts to compute
		// the Fibonacci sequence recursively. Due to
		// memoization, this should run in polynomial time.
		computer := NewMockComputerForTesting(ctrl)
		computer.EXPECT().ComputeMessageValue(gomock.Any(), gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, key model_core.Message[proto.Message, object.LocalReference], e model_evaluation.Environment[object.LocalReference, model_core.ReferenceMetadata]) (model_core.PatchedMessage[proto.Message, model_core.ReferenceMetadata], error) {
				// Base case: fib(0) and fib(1).
				k := key.Message.(*wrapperspb.UInt32Value)
				if k.Value <= 1 {
					return model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](
						&wrapperspb.UInt64Value{
							Value: uint64(k.Value),
						},
					), nil
				}

				// Recursion: fib(n) = fib(n-2) + fib(n-1).
				v0 := e.GetMessageValue(model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](&wrapperspb.UInt32Value{
					Value: k.Value - 2,
				}))
				v1 := e.GetMessageValue(model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](&wrapperspb.UInt32Value{
					Value: k.Value - 1,
				}))
				if !v0.IsSet() || !v1.IsSet() {
					return model_core.PatchedMessage[proto.Message, model_core.ReferenceMetadata]{}, model_evaluation.ErrMissingDependency
				}
				return model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](
					&wrapperspb.UInt64Value{
						Value: v0.Message.(*wrapperspb.UInt64Value).Value + v1.Message.(*wrapperspb.UInt64Value).Value,
					},
				), nil
			}).
			AnyTimes()
		computer.EXPECT().IsLookup("type.googleapis.com/google.protobuf.UInt32Value").Return(false).AnyTimes()
		objectManager := NewMockObjectManagerForTesting(ctrl)
		tagStore := NewMockBoundStoreForTesting(ctrl)
		tagStore.EXPECT().ResolveTag(gomock.Any(), gomock.Any()).Return(object.LocalReference{}, status.Error(codes.NotFound, "Tag does not exist")).AnyTimes()
		evaluationReader := NewMockProtoEvaluationReaderForTesting(ctrl)
		lookupResultReader := NewMockLookupResultReaderForTesting(ctrl)
		lookupResultReader.EXPECT().GetDecodingParametersSizeBytes().Return(16).AnyTimes()
		keysReader := NewMockKeysReaderForTesting(ctrl)
		cacheDeterministicEncoder := model_encoding.NewChainedDeterministicBinaryEncoder(nil)
		cacheKeyedEncoder := NewMockKeyedBinaryEncoder(ctrl)

		queuesFactory := model_evaluation.NewSimpleRecursiveComputerEvaluationQueuesFactory[object.LocalReference, model_core.ReferenceMetadata](1)
		queues := queuesFactory.NewQueues()
		recursiveComputer := model_evaluation.NewRecursiveComputer(
			computer,
			queues,
			object.SHA256V1ReferenceFormat,
			objectManager,
			tagStore,
			/* actionTagKeyReference = */ object.MustNewSHA256V1LocalReference("f07997aa26d63ad33c8b2e6f920ae9b42c93bacb67c84ae529d065c6d572d342", 2323, 0, 0, 0),
			evaluationReader,
			lookupResultReader,
			keysReader,
			cacheDeterministicEncoder,
			cacheKeyedEncoder,
			clock.SystemClock,
		)

		computer.EXPECT().ReturnsNativeValue("type.googleapis.com/google.protobuf.UInt32Value").Return(false)
		keyState, err := recursiveComputer.GetOrCreateKeyState(
			util.Must(
				model_core.MarshalTopLevelAny(
					model_core.NewSimpleTopLevelMessage[object.LocalReference, proto.Message](
						&wrapperspb.UInt32Value{
							Value: 93,
						},
					),
				),
			),
		)
		require.NoError(t, err)

		require.NoError(
			t,
			program.RunLocal(ctx, func(ctx context.Context, siblingsGroup, dependenciesGroup program.Group) error {
				queues.ProcessAllEvaluatableKeys(dependenciesGroup, recursiveComputer)
				return recursiveComputer.WaitForEvaluation(ctx, keyState)
			}),
		)

		objectManager.EXPECT().CaptureCreatedObject(gomock.Any(), gomock.Any()).
			Return(model_core.NoopReferenceMetadata{}, nil).
			AnyTimes()
		objectManager.EXPECT().CaptureExistingObject(gomock.Any()).
			Return(model_core.NoopReferenceMetadata{}).
			AnyTimes()
		objectManager.EXPECT().ReferenceObject(gomock.Any()).
			DoAndReturn(func(metadataEntry model_core.MetadataEntry[model_core.ReferenceMetadata]) object.LocalReference {
				return metadataEntry.LocalReference
			}).
			AnyTimes()

		patchedEvaluations, err := recursiveComputer.GetEvaluations(
			ctx,
			[]*model_evaluation.KeyState[object.LocalReference, model_core.ReferenceMetadata]{
				keyState,
			},
		)
		require.NoError(t, err)

		// TODO: Validate references. Length is incorrect!
		evaluations, _ := patchedEvaluations.SortAndSetReferences()
		require.Len(t, evaluations.Message, 0)

		/*
			testutil.RequireEqualProto(t, &model_evaluation_pb.Evaluations{
				Level: &model_evaluation_pb.Evaluations_Leaf_{
					Leaf: &model_evaluation_pb.Evaluations_Leaf{
						// &wrapperspb.UInt32Value{Value: 93}
						KeyReference: []byte{
							0x0c, 0xfe, 0x3c, 0x1c, 0x36, 0x0e, 0x96, 0x44,
							0x16, 0xc0, 0x88, 0xf3, 0xfb, 0x93, 0xd4, 0x68,
							0xbe, 0x6a, 0xfd, 0xe0, 0x35, 0xc4, 0x8e, 0xf4,
							0x41, 0xe7, 0x61, 0x11, 0xa8, 0x1b, 0xe3, 0xdd,
							0x35, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
						},
						Graphlet: &model_evaluation_pb.Graphlet{
							Value: &model_core_pb.Any{
								Value: util.Must(anypb.New(&wrapperspb.UInt64Value{
									Value: 12200160415121876738,
								})),
								References: &model_core_pb.ReferenceSet{},
							},
							DirectVariableDependencyKeys: []*model_evaluation_pb.Keys{
								{
									Level: &model_evaluation_pb.Keys_Leaf{
										Leaf: &model_core_pb.Any{
											Value: util.Must(anypb.New(&wrapperspb.UInt32Value{
												Value: 92,
											})),
											References: &model_core_pb.ReferenceSet{},
										},
									},
								},
								{
									Level: &model_evaluation_pb.Keys_Leaf{
										Leaf: &model_core_pb.Any{
											Value: util.Must(anypb.New(&wrapperspb.UInt32Value{
												Value: 91,
											})),
											References: &model_core_pb.ReferenceSet{},
										},
									},
								},
							},
							// TODO: DependencyEvaluations: []*model_evaluation_pb.Evaluations{},
						},
					},
				},
			}, evaluations.Message[0])
		*/
	})

	t.Run("Cycle", func(t *testing.T) {
		// Provide a computer that does nothing more than
		// request its own value recursively. This should
		// immediately trigger cycle detection.
		computer := NewMockComputerForTesting(ctrl)
		computer.EXPECT().ComputeMessageValue(gomock.Any(), gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, key model_core.Message[proto.Message, object.LocalReference], e model_evaluation.Environment[object.LocalReference, model_core.ReferenceMetadata]) (model_core.PatchedMessage[proto.Message, model_core.ReferenceMetadata], error) {
				v := e.GetMessageValue(model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata](key.Message))
				require.False(t, v.IsSet())
				return model_core.PatchedMessage[proto.Message, model_core.ReferenceMetadata]{}, model_evaluation.ErrMissingDependency
			})
		computer.EXPECT().IsLookup("type.googleapis.com/google.protobuf.UInt32Value").Return(false).AnyTimes()
		objectManager := NewMockObjectManagerForTesting(ctrl)
		tagStore := NewMockBoundStoreForTesting(ctrl)
		tagStore.EXPECT().ResolveTag(gomock.Any(), gomock.Any()).Return(object.LocalReference{}, status.Error(codes.NotFound, "Tag does not exist")).AnyTimes()
		evaluationReader := NewMockProtoEvaluationReaderForTesting(ctrl)
		lookupResultReader := NewMockLookupResultReaderForTesting(ctrl)
		lookupResultReader.EXPECT().GetDecodingParametersSizeBytes().Return(16).AnyTimes()
		keysReader := NewMockKeysReaderForTesting(ctrl)
		cacheDeterministicEncoder := NewMockDeterministicBinaryEncoder(ctrl)
		cacheKeyedEncoder := NewMockKeyedBinaryEncoder(ctrl)

		queuesFactory := model_evaluation.NewSimpleRecursiveComputerEvaluationQueuesFactory[object.LocalReference, model_core.ReferenceMetadata](1)
		queues := queuesFactory.NewQueues()
		recursiveComputer := model_evaluation.NewRecursiveComputer(
			computer,
			queues,
			object.SHA256V1ReferenceFormat,
			objectManager,
			tagStore,
			/* actionTagKeyReference = */ object.MustNewSHA256V1LocalReference("0e847672a7a34ba848ec92f4000a9f86049e5557496cfcede0db7744bf77c12b", 8575, 0, 0, 0),
			evaluationReader,
			lookupResultReader,
			keysReader,
			cacheDeterministicEncoder,
			cacheKeyedEncoder,
			clock.SystemClock,
		)

		computer.EXPECT().ReturnsNativeValue("type.googleapis.com/google.protobuf.UInt32Value").Return(false)
		keyState, err := recursiveComputer.GetOrCreateKeyState(
			util.Must(
				model_core.MarshalTopLevelAny(
					model_core.NewSimpleTopLevelMessage[object.LocalReference, proto.Message](
						&wrapperspb.UInt32Value{
							Value: 42,
						},
					),
				),
			),
		)
		require.NoError(t, err)

		require.Equal(
			t,
			model_evaluation.NestedError[object.LocalReference, model_core.ReferenceMetadata]{
				KeyState: keyState,
				Err:      errors.New("cyclic dependency detected"),
			},
			program.RunLocal(ctx, func(ctx context.Context, siblingsGroup, dependenciesGroup program.Group) error {
				queues.ProcessAllEvaluatableKeys(dependenciesGroup, recursiveComputer)
				return recursiveComputer.WaitForEvaluation(ctx, keyState)
			}),
		)
	})

	t.Run("ErrorPropagation", func(t *testing.T) {
		// Have a simple function that calls a couple of levels
		// deep. At some point this will return an error, which
		// should be propagated all the way back up.
		computer := NewMockComputerForTesting(ctrl)
		computer.EXPECT().ComputeMessageValue(gomock.Any(), gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, key model_core.Message[proto.Message, object.LocalReference], e model_evaluation.Environment[object.LocalReference, model_core.ReferenceMetadata]) (model_core.PatchedMessage[proto.Message, model_core.ReferenceMetadata], error) {
				k := key.Message.(*wrapperspb.UInt32Value)
				if k.Value == 0 {
					return model_core.PatchedMessage[proto.Message, model_core.ReferenceMetadata]{}, errors.New("reached zero")
				}

				v := e.GetMessageValue(model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](&wrapperspb.UInt32Value{
					Value: k.Value - 1,
				}))
				if !v.IsSet() {
					return model_core.PatchedMessage[proto.Message, model_core.ReferenceMetadata]{}, model_evaluation.ErrMissingDependency
				}
				return model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](
					&emptypb.Empty{},
				), nil
			}).
			Times(6)
		computer.EXPECT().IsLookup("type.googleapis.com/google.protobuf.UInt32Value").Return(false).AnyTimes()
		objectManager := NewMockObjectManagerForTesting(ctrl)
		tagStore := NewMockBoundStoreForTesting(ctrl)
		tagStore.EXPECT().ResolveTag(gomock.Any(), gomock.Any()).Return(object.LocalReference{}, status.Error(codes.NotFound, "Tag does not exist")).AnyTimes()
		evaluationReader := NewMockProtoEvaluationReaderForTesting(ctrl)
		lookupResultReader := NewMockLookupResultReaderForTesting(ctrl)
		lookupResultReader.EXPECT().GetDecodingParametersSizeBytes().Return(16).AnyTimes()
		keysReader := NewMockKeysReaderForTesting(ctrl)
		cacheDeterministicEncoder := NewMockDeterministicBinaryEncoder(ctrl)
		cacheKeyedEncoder := NewMockKeyedBinaryEncoder(ctrl)

		queuesFactory := model_evaluation.NewSimpleRecursiveComputerEvaluationQueuesFactory[object.LocalReference, model_core.ReferenceMetadata](1)
		queues := queuesFactory.NewQueues()
		recursiveComputer := model_evaluation.NewRecursiveComputer(
			computer,
			queues,
			object.SHA256V1ReferenceFormat,
			objectManager,
			tagStore,
			/* actionTagKeyReference = */ object.MustNewSHA256V1LocalReference("e5283197708f96f2368701a89fcdd72367106497f0335bd2d5f3403a826d71da", 8584, 0, 0, 0),
			evaluationReader,
			lookupResultReader,
			keysReader,
			cacheDeterministicEncoder,
			cacheKeyedEncoder,
			clock.SystemClock,
		)

		computer.EXPECT().ReturnsNativeValue("type.googleapis.com/google.protobuf.UInt32Value").Return(false)
		keyState2, err := recursiveComputer.GetOrCreateKeyState(
			util.Must(
				model_core.MarshalTopLevelAny(
					model_core.NewSimpleTopLevelMessage[object.LocalReference, proto.Message](
						&wrapperspb.UInt32Value{
							Value: 2,
						},
					),
				),
			),
		)
		require.NoError(t, err)

		computer.EXPECT().ReturnsNativeValue("type.googleapis.com/google.protobuf.UInt32Value").Return(false)
		errCompute := program.RunLocal(ctx, func(ctx context.Context, siblingsGroup, dependenciesGroup program.Group) error {
			queues.ProcessAllEvaluatableKeys(dependenciesGroup, recursiveComputer)
			return recursiveComputer.WaitForEvaluation(ctx, keyState2)
		})

		computer.EXPECT().ReturnsNativeValue("type.googleapis.com/google.protobuf.UInt32Value").Return(false)
		keyState0, err := recursiveComputer.GetOrCreateKeyState(
			util.Must(
				model_core.MarshalTopLevelAny(
					model_core.NewSimpleTopLevelMessage[object.LocalReference, proto.Message](
						&wrapperspb.UInt32Value{
							Value: 0,
						},
					),
				),
			),
		)
		require.NoError(t, err)

		computer.EXPECT().ReturnsNativeValue("type.googleapis.com/google.protobuf.UInt32Value").Return(false)
		keyState1, err := recursiveComputer.GetOrCreateKeyState(
			util.Must(
				model_core.MarshalTopLevelAny(
					model_core.NewSimpleTopLevelMessage[object.LocalReference, proto.Message](
						&wrapperspb.UInt32Value{
							Value: 1,
						},
					),
				),
			),
		)
		require.NoError(t, err)

		require.Equal(
			t,
			model_evaluation.NestedError[object.LocalReference, model_core.ReferenceMetadata]{
				KeyState: keyState1,
				Err: model_evaluation.NestedError[object.LocalReference, model_core.ReferenceMetadata]{
					KeyState: keyState0,
					Err:      errors.New("reached zero"),
				},
			},
			errCompute,
		)

		// A subsequent evaluation of a new key that depends on
		// a previously computed key that is already in the
		// error state should also propagate the error.
		keyState3, err := recursiveComputer.GetOrCreateKeyState(
			util.Must(
				model_core.MarshalTopLevelAny(
					model_core.NewSimpleTopLevelMessage[object.LocalReference, proto.Message](
						&wrapperspb.UInt32Value{
							Value: 3,
						},
					),
				),
			),
		)
		require.NoError(t, err)
		require.Equal(
			t,
			model_evaluation.NestedError[object.LocalReference, model_core.ReferenceMetadata]{
				KeyState: keyState2,
				Err: model_evaluation.NestedError[object.LocalReference, model_core.ReferenceMetadata]{
					KeyState: keyState1,
					Err: model_evaluation.NestedError[object.LocalReference, model_core.ReferenceMetadata]{
						KeyState: keyState0,
						Err:      errors.New("reached zero"),
					},
				},
			},
			program.RunLocal(ctx, func(ctx context.Context, siblingsGroup, dependenciesGroup program.Group) error {
				queues.ProcessAllEvaluatableKeys(dependenciesGroup, recursiveComputer)
				return recursiveComputer.WaitForEvaluation(ctx, keyState3)
			}),
		)
	})

	t.Run("CacheLookupNotFound", func(t *testing.T) {
		// For every key for which evaluation is requested, we
		// should first see a call to ResolveTag() to obtain the
		// set of expected dependencies. If this returns
		// NOT_FOUND, evaluation should be performed.
		computer := NewMockComputerForTesting(ctrl)
		computer.EXPECT().IsLookup("type.googleapis.com/google.protobuf.UInt32Value").Return(false).AnyTimes()
		objectManager := NewMockObjectManagerForTesting(ctrl)
		tagStore := NewMockBoundStoreForTesting(ctrl)
		evaluationReader := NewMockProtoEvaluationReaderForTesting(ctrl)
		lookupResultReader := NewMockLookupResultReaderForTesting(ctrl)
		lookupResultReader.EXPECT().GetDecodingParametersSizeBytes().Return(16).AnyTimes()
		keysReader := NewMockKeysReaderForTesting(ctrl)
		cacheDeterministicEncoder := NewMockDeterministicBinaryEncoder(ctrl)
		cacheKeyedEncoder := NewMockKeyedBinaryEncoder(ctrl)

		queuesFactory := model_evaluation.NewSimpleRecursiveComputerEvaluationQueuesFactory[object.LocalReference, model_core.ReferenceMetadata](1)
		queues := queuesFactory.NewQueues()
		recursiveComputer := model_evaluation.NewRecursiveComputer(
			computer,
			queues,
			object.SHA256V1ReferenceFormat,
			objectManager,
			tagStore,
			/* actionTagKeyReference = */ object.MustNewSHA256V1LocalReference("10479a81dafa74a2f72438cbce7d472cb9e8ea3648a9a2ec3622a27620a02925", 23721, 0, 0, 0),
			evaluationReader,
			lookupResultReader,
			keysReader,
			cacheDeterministicEncoder,
			cacheKeyedEncoder,
			clock.SystemClock,
		)

		computer.EXPECT().ReturnsNativeValue("type.googleapis.com/google.protobuf.UInt32Value").Return(false)
		keyState, err := recursiveComputer.GetOrCreateKeyState(
			util.Must(
				model_core.MarshalTopLevelAny(
					model_core.NewSimpleTopLevelMessage[object.LocalReference, proto.Message](
						&wrapperspb.UInt32Value{
							Value: 67,
						},
					),
				),
			),
		)
		require.NoError(t, err)

		tagStore.EXPECT().ResolveTag(
			gomock.Any(),
			[...]byte{
				0xbf, 0xba, 0xc9, 0x92, 0x7b, 0xff, 0x1f, 0xfe,
				0xd1, 0x95, 0x69, 0xab, 0x3a, 0x78, 0x6c, 0xf7,
				0x56, 0x95, 0x13, 0x19, 0x5f, 0xbe, 0x80, 0xc5,
				0x74, 0x76, 0x7c, 0x78, 0xea, 0x47, 0x94, 0xc6,
			},
		).Return(object.LocalReference{}, status.Error(codes.NotFound, "Tag does not exist"))
		computer.EXPECT().ComputeMessageValue(gomock.Any(), gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, key model_core.Message[proto.Message, object.LocalReference], e model_evaluation.Environment[object.LocalReference, model_core.ReferenceMetadata]) (model_core.PatchedMessage[proto.Message, model_core.ReferenceMetadata], error) {
				return model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](
					&wrapperspb.UInt64Value{Value: 42},
				), nil
			})

		require.NoError(
			t,
			program.RunLocal(ctx, func(ctx context.Context, siblingsGroup, dependenciesGroup program.Group) error {
				queues.ProcessAllEvaluatableKeys(dependenciesGroup, recursiveComputer)
				return recursiveComputer.WaitForEvaluation(ctx, keyState)
			}),
		)
		/*
			testutil.RequireEqualProto(t, &wrapperspb.UInt64Value{
				Value: 42,
			}, value.Message)
		*/
	})

	t.Run("CacheLookupReadNotFound", func(t *testing.T) {
		// Even though we require that tags of cached results
		// resolve to complete graphs, flaky storage may always
		// cause subsequent reads to return NOT_FOUND. In such
		// cases we simply assume there's a cache miss.
		computer := NewMockComputerForTesting(ctrl)
		computer.EXPECT().IsLookup("type.googleapis.com/google.protobuf.UInt32Value").Return(false).AnyTimes()
		objectManager := NewMockObjectManagerForTesting(ctrl)
		tagStore := NewMockBoundStoreForTesting(ctrl)
		evaluationReader := NewMockProtoEvaluationReaderForTesting(ctrl)
		lookupResultReader := NewMockLookupResultReaderForTesting(ctrl)
		lookupResultReader.EXPECT().GetDecodingParametersSizeBytes().Return(16).AnyTimes()
		keysReader := NewMockKeysReaderForTesting(ctrl)
		cacheDeterministicEncoder := NewMockDeterministicBinaryEncoder(ctrl)
		cacheKeyedEncoder := NewMockKeyedBinaryEncoder(ctrl)

		queuesFactory := model_evaluation.NewSimpleRecursiveComputerEvaluationQueuesFactory[object.LocalReference, model_core.ReferenceMetadata](1)
		queues := queuesFactory.NewQueues()
		recursiveComputer := model_evaluation.NewRecursiveComputer(
			computer,
			queues,
			object.SHA256V1ReferenceFormat,
			objectManager,
			tagStore,
			/* actionTagKeyReference = */ object.MustNewSHA256V1LocalReference("0c1c0eacebd721476e1a158da2e3ee0281c9c6fe9e8f9e2941f2a05153869b32", 48374, 0, 0, 0),
			evaluationReader,
			lookupResultReader,
			keysReader,
			cacheDeterministicEncoder,
			cacheKeyedEncoder,
			clock.SystemClock,
		)

		computer.EXPECT().ReturnsNativeValue("type.googleapis.com/google.protobuf.UInt32Value").Return(false)
		keyState, err := recursiveComputer.GetOrCreateKeyState(
			util.Must(
				model_core.MarshalTopLevelAny(
					model_core.NewSimpleTopLevelMessage[object.LocalReference, proto.Message](
						&wrapperspb.UInt32Value{
							Value: 67,
						},
					),
				),
			),
		)
		require.NoError(t, err)

		tagStore.EXPECT().ResolveTag(
			gomock.Any(),
			[...]byte{
				0x96, 0x82, 0x42, 0x1e, 0x5e, 0x3e, 0x71, 0x8b,
				0x56, 0x4f, 0x6a, 0xbe, 0x93, 0x0f, 0x90, 0x60,
				0x70, 0xf9, 0xef, 0xe8, 0x25, 0x70, 0x88, 0x90,
				0xc4, 0x9e, 0xed, 0x80, 0x6e, 0x92, 0xd1, 0x17,
			},
		).Return(object.MustNewSHA256V1LocalReference("12271f9d66852891725166bd460656b9a2b9a5c6697a51311963ac7d5acd2d0c", 48374, 0, 0, 0), nil)
		lookupResultReader.EXPECT().ReadObject(
			gomock.Any(),
			util.Must(
				model_core.NewDecodable(
					object.MustNewSHA256V1LocalReference("12271f9d66852891725166bd460656b9a2b9a5c6697a51311963ac7d5acd2d0c", 48374, 0, 0, 0),
					[]byte{
						0x63, 0xa0, 0x43, 0x40, 0xfc, 0x0b, 0x58, 0x14,
						0x7f, 0x9d, 0x56, 0x84, 0x39, 0xff, 0x39, 0x12,
					},
				),
			),
		).Return(model_core.Message[*model_evaluation_cache_pb.LookupResult, object.LocalReference]{}, status.Error(codes.NotFound, "Object does not exist"))
		computer.EXPECT().ComputeMessageValue(gomock.Any(), gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, key model_core.Message[proto.Message, object.LocalReference], e model_evaluation.Environment[object.LocalReference, model_core.ReferenceMetadata]) (model_core.PatchedMessage[proto.Message, model_core.ReferenceMetadata], error) {
				return model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](
					&wrapperspb.UInt64Value{Value: 42},
				), nil
			})

		require.NoError(
			t,
			program.RunLocal(ctx, func(ctx context.Context, siblingsGroup, dependenciesGroup program.Group) error {
				queues.ProcessAllEvaluatableKeys(dependenciesGroup, recursiveComputer)
				return recursiveComputer.WaitForEvaluation(ctx, keyState)
			}),
		)
		/*
			testutil.RequireEqualProto(t, &wrapperspb.UInt64Value{
				Value: 42,
			}, value.Message)
		*/
	})

	t.Run("CacheLookupReadFailedPrecondition", func(t *testing.T) {
		// Storage backends that are expected to hold all
		// referenced objects report missing ones as
		// FAILED_PRECONDITION (see
		// pkg/storage/object/existenceprecondition). If reading
		// a cached lookup result fails that way because the
		// object got lost by storage, we should fall back to
		// computing the value, just like for NOT_FOUND.
		computer := NewMockComputerForTesting(ctrl)
		computer.EXPECT().IsLookup("type.googleapis.com/google.protobuf.UInt32Value").Return(false).AnyTimes()
		objectManager := NewMockObjectManagerForTesting(ctrl)
		tagStore := NewMockBoundStoreForTesting(ctrl)
		evaluationReader := NewMockProtoEvaluationReaderForTesting(ctrl)
		lookupResultReader := NewMockLookupResultReaderForTesting(ctrl)
		lookupResultReader.EXPECT().GetDecodingParametersSizeBytes().Return(16).AnyTimes()
		keysReader := NewMockKeysReaderForTesting(ctrl)
		cacheDeterministicEncoder := NewMockDeterministicBinaryEncoder(ctrl)
		cacheKeyedEncoder := NewMockKeyedBinaryEncoder(ctrl)

		queuesFactory := model_evaluation.NewSimpleRecursiveComputerEvaluationQueuesFactory[object.LocalReference, model_core.ReferenceMetadata](1)
		queues := queuesFactory.NewQueues()
		recursiveComputer := model_evaluation.NewRecursiveComputer(
			computer,
			queues,
			object.SHA256V1ReferenceFormat,
			objectManager,
			tagStore,
			/* actionTagKeyReference = */ object.MustNewSHA256V1LocalReference("0c1c0eacebd721476e1a158da2e3ee0281c9c6fe9e8f9e2941f2a05153869b32", 48374, 0, 0, 0),
			evaluationReader,
			lookupResultReader,
			keysReader,
			cacheDeterministicEncoder,
			cacheKeyedEncoder,
			clock.SystemClock,
		)

		computer.EXPECT().ReturnsNativeValue("type.googleapis.com/google.protobuf.UInt32Value").Return(false)
		keyState, err := recursiveComputer.GetOrCreateKeyState(
			util.Must(
				model_core.MarshalTopLevelAny(
					model_core.NewSimpleTopLevelMessage[object.LocalReference, proto.Message](
						&wrapperspb.UInt32Value{
							Value: 67,
						},
					),
				),
			),
		)
		require.NoError(t, err)

		tagStore.EXPECT().ResolveTag(
			gomock.Any(),
			[...]byte{
				0x96, 0x82, 0x42, 0x1e, 0x5e, 0x3e, 0x71, 0x8b,
				0x56, 0x4f, 0x6a, 0xbe, 0x93, 0x0f, 0x90, 0x60,
				0x70, 0xf9, 0xef, 0xe8, 0x25, 0x70, 0x88, 0x90,
				0xc4, 0x9e, 0xed, 0x80, 0x6e, 0x92, 0xd1, 0x17,
			},
		).Return(object.MustNewSHA256V1LocalReference("12271f9d66852891725166bd460656b9a2b9a5c6697a51311963ac7d5acd2d0c", 48374, 0, 0, 0), nil)
		lookupResultReader.EXPECT().ReadObject(
			gomock.Any(),
			util.Must(
				model_core.NewDecodable(
					object.MustNewSHA256V1LocalReference("12271f9d66852891725166bd460656b9a2b9a5c6697a51311963ac7d5acd2d0c", 48374, 0, 0, 0),
					[]byte{
						0x63, 0xa0, 0x43, 0x40, 0xfc, 0x0b, 0x58, 0x14,
						0x7f, 0x9d, 0x56, 0x84, 0x39, 0xff, 0x39, 0x12,
					},
				),
			),
		).Return(model_core.Message[*model_evaluation_cache_pb.LookupResult, object.LocalReference]{}, status.Error(codes.FailedPrecondition, "Record not found"))
		computer.EXPECT().ComputeMessageValue(gomock.Any(), gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, key model_core.Message[proto.Message, object.LocalReference], e model_evaluation.Environment[object.LocalReference, model_core.ReferenceMetadata]) (model_core.PatchedMessage[proto.Message, model_core.ReferenceMetadata], error) {
				return model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](
					&wrapperspb.UInt64Value{Value: 42},
				), nil
			})

		require.NoError(
			t,
			program.RunLocal(ctx, func(ctx context.Context, siblingsGroup, dependenciesGroup program.Group) error {
				queues.ProcessAllEvaluatableKeys(dependenciesGroup, recursiveComputer)
				return recursiveComputer.WaitForEvaluation(ctx, keyState)
			}),
		)
	})

	t.Run("CachedValueMissingObjectTriggersRecompute", func(t *testing.T) {
		// A key whose value was served from the cache may fail
		// to be read later on, because storage lost objects it
		// references. When that happens while another key
		// consumes its value, the cached value should be
		// invalidated and recomputed, and the consuming key
		// should be retried afterwards.
		computer := NewMockComputerForTesting(ctrl)
		computer.EXPECT().IsLookup("type.googleapis.com/google.protobuf.UInt32Value").Return(false).AnyTimes()
		objectManager := NewMockObjectManagerForTesting(ctrl)
		tagStore := NewMockBoundStoreForTesting(ctrl)
		evaluationReader := NewMockProtoEvaluationReaderForTesting(ctrl)
		lookupResultReader := NewMockLookupResultReaderForTesting(ctrl)
		lookupResultReader.EXPECT().GetDecodingParametersSizeBytes().Return(16).AnyTimes()
		keysReader := NewMockKeysReaderForTesting(ctrl)
		cacheDeterministicEncoder := NewMockDeterministicBinaryEncoder(ctrl)
		cacheKeyedEncoder := NewMockKeyedBinaryEncoder(ctrl)

		queuesFactory := model_evaluation.NewSimpleRecursiveComputerEvaluationQueuesFactory[object.LocalReference, model_core.ReferenceMetadata](1)
		queues := queuesFactory.NewQueues()
		recursiveComputer := model_evaluation.NewRecursiveComputer(
			computer,
			queues,
			object.SHA256V1ReferenceFormat,
			objectManager,
			tagStore,
			/* actionTagKeyReference = */ object.MustNewSHA256V1LocalReference("31f4e64c9856ac7c7a54a6ba00e05a2b22e7fb14f929506e2c22b1ba1b770ecd", 3874, 0, 0, 0),
			evaluationReader,
			lookupResultReader,
			keysReader,
			cacheDeterministicEncoder,
			cacheKeyedEncoder,
			clock.SystemClock,
		)

		// Key 1 is computed and depends on key 2, whose initial
		// cache lookup yields a value immediately.
		gomock.InOrder(
			tagStore.EXPECT().ResolveTag(gomock.Any(), gomock.Any()).
				Return(object.LocalReference{}, status.Error(codes.NotFound, "Tag does not exist")),
			tagStore.EXPECT().ResolveTag(gomock.Any(), gomock.Any()).
				Return(object.MustNewSHA256V1LocalReference("53a3478e21b1e33fc50fbb8dd4b9dad1de9a033eab98b95a4b90a9d6a4884784", 3874, 0, 0, 0), nil),
		)
		// The first read of key 2's cached value succeeds, but
		// the second one fails because storage lost objects.
		// This should cause key 2 to be recomputed exactly once,
		// without performing any more cache lookups.
		gomock.InOrder(
			lookupResultReader.EXPECT().ReadObject(gomock.Any(), gomock.Any()).
				Return(model_core.NewSimpleMessage[object.LocalReference](
					&model_evaluation_cache_pb.LookupResult{
						Result: &model_evaluation_cache_pb.LookupResult_HitValue{
							HitValue: &model_core_pb.Any{
								Value: util.Must(anypb.New(&wrapperspb.UInt64Value{
									Value: 7,
								})),
								References: &model_core_pb.ReferenceSet{},
							},
						},
					},
				), nil),
			lookupResultReader.EXPECT().ReadObject(gomock.Any(), gomock.Any()).
				Return(model_core.Message[*model_evaluation_cache_pb.LookupResult, object.LocalReference]{}, status.Error(codes.FailedPrecondition, "Record not found")),
		)
		computeMessageValue := func(ctx context.Context, key model_core.Message[proto.Message, object.LocalReference], e model_evaluation.Environment[object.LocalReference, model_core.ReferenceMetadata]) (model_core.PatchedMessage[proto.Message, model_core.ReferenceMetadata], error) {
			k := key.Message.(*wrapperspb.UInt32Value)
			if k.Value == 2 {
				return model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](
					&wrapperspb.UInt64Value{Value: 8},
				), nil
			}
			v := e.GetMessageValue(model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](&wrapperspb.UInt32Value{
				Value: 2,
			}))
			if !v.IsSet() {
				return model_core.PatchedMessage[proto.Message, model_core.ReferenceMetadata]{}, model_evaluation.ErrMissingDependency
			}
			return model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](
				&wrapperspb.UInt64Value{
					Value: v.Message.(*wrapperspb.UInt64Value).Value + 1,
				},
			), nil
		}
		// Key 1 is evaluated three times: first it observes key
		// 2 to be missing, then reading key 2's cached value
		// fails, and finally it observes the recomputed value.
		// Key 2 is only computed once, after invalidation.
		computer.EXPECT().ComputeMessageValue(gomock.Any(), gomock.Any(), gomock.Any()).
			DoAndReturn(computeMessageValue).
			Times(4)

		computer.EXPECT().ReturnsNativeValue("type.googleapis.com/google.protobuf.UInt32Value").Return(false)
		keyState, err := recursiveComputer.GetOrCreateKeyState(
			util.Must(
				model_core.MarshalTopLevelAny(
					model_core.NewSimpleTopLevelMessage[object.LocalReference, proto.Message](
						&wrapperspb.UInt32Value{
							Value: 1,
						},
					),
				),
			),
		)
		require.NoError(t, err)

		require.NoError(
			t,
			program.RunLocal(ctx, func(ctx context.Context, siblingsGroup, dependenciesGroup program.Group) error {
				queues.ProcessAllEvaluatableKeys(dependenciesGroup, recursiveComputer)
				return recursiveComputer.WaitForEvaluation(ctx, keyState)
			}),
		)
	})

	t.Run("ComputeMissingObjectRetries", func(t *testing.T) {
		// Objects belonging to a cached dependency's value may
		// only be dereferenced deep inside the compute function
		// of a consuming key, in which case the resulting
		// FAILED_PRECONDITION error cannot be attributed to a
		// single dependency read. All cache-backed dependencies
		// consumed during the attempt should be invalidated and
		// the key retried.
		computer := NewMockComputerForTesting(ctrl)
		computer.EXPECT().IsLookup("type.googleapis.com/google.protobuf.UInt32Value").Return(false).AnyTimes()
		objectManager := NewMockObjectManagerForTesting(ctrl)
		tagStore := NewMockBoundStoreForTesting(ctrl)
		evaluationReader := NewMockProtoEvaluationReaderForTesting(ctrl)
		lookupResultReader := NewMockLookupResultReaderForTesting(ctrl)
		lookupResultReader.EXPECT().GetDecodingParametersSizeBytes().Return(16).AnyTimes()
		keysReader := NewMockKeysReaderForTesting(ctrl)
		cacheDeterministicEncoder := NewMockDeterministicBinaryEncoder(ctrl)
		cacheKeyedEncoder := NewMockKeyedBinaryEncoder(ctrl)

		queuesFactory := model_evaluation.NewSimpleRecursiveComputerEvaluationQueuesFactory[object.LocalReference, model_core.ReferenceMetadata](1)
		queues := queuesFactory.NewQueues()
		recursiveComputer := model_evaluation.NewRecursiveComputer(
			computer,
			queues,
			object.SHA256V1ReferenceFormat,
			objectManager,
			tagStore,
			/* actionTagKeyReference = */ object.MustNewSHA256V1LocalReference("b8809d11c4b729bf22cf1e30c2226b3644fca1e6a7c02fa4a1a5b0e0ba9e5d3f", 39218, 0, 0, 0),
			evaluationReader,
			lookupResultReader,
			keysReader,
			cacheDeterministicEncoder,
			cacheKeyedEncoder,
			clock.SystemClock,
		)

		// Key 1 is computed and depends on key 2, whose value is
		// served from the cache. Reads of key 2's cached value
		// succeed: once during key 2's initial lookup and once
		// when key 1 consumes its value.
		gomock.InOrder(
			tagStore.EXPECT().ResolveTag(gomock.Any(), gomock.Any()).
				Return(object.LocalReference{}, status.Error(codes.NotFound, "Tag does not exist")),
			tagStore.EXPECT().ResolveTag(gomock.Any(), gomock.Any()).
				Return(object.MustNewSHA256V1LocalReference("a48f24425cd76e4d3b4bd1d6dfbb4cc4d4b0e37e6d5f2f1a9622c07dbd4a06f8", 39218, 0, 0, 0), nil),
		)
		lookupResultReader.EXPECT().ReadObject(gomock.Any(), gomock.Any()).
			Return(model_core.NewSimpleMessage[object.LocalReference](
				&model_evaluation_cache_pb.LookupResult{
					Result: &model_evaluation_cache_pb.LookupResult_HitValue{
						HitValue: &model_core_pb.Any{
							Value: util.Must(anypb.New(&wrapperspb.UInt64Value{
								Value: 7,
							})),
							References: &model_core_pb.ReferenceSet{},
						},
					},
				},
			), nil).
			Times(2)
		// Key 1's compute function fails with
		// FAILED_PRECONDITION on its first full attempt, as if
		// it dereferenced a missing object nested inside key 2's
		// value. This should invalidate key 2 and retry key 1.
		// Key 1 is evaluated three times: once observing key 2
		// to be missing, once failing, and once succeeding
		// against the recomputed value. Key 2 is computed once.
		attempts := 0
		computer.EXPECT().ComputeMessageValue(gomock.Any(), gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, key model_core.Message[proto.Message, object.LocalReference], e model_evaluation.Environment[object.LocalReference, model_core.ReferenceMetadata]) (model_core.PatchedMessage[proto.Message, model_core.ReferenceMetadata], error) {
				k := key.Message.(*wrapperspb.UInt32Value)
				if k.Value == 2 {
					return model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](
						&wrapperspb.UInt64Value{Value: 8},
					), nil
				}
				v := e.GetMessageValue(model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](&wrapperspb.UInt32Value{
					Value: 2,
				}))
				if !v.IsSet() {
					return model_core.PatchedMessage[proto.Message, model_core.ReferenceMetadata]{}, model_evaluation.ErrMissingDependency
				}
				attempts++
				if attempts == 1 {
					return model_core.PatchedMessage[proto.Message, model_core.ReferenceMetadata]{}, status.Error(codes.FailedPrecondition, "Record not found")
				}
				return model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](
					&wrapperspb.UInt64Value{
						Value: v.Message.(*wrapperspb.UInt64Value).Value + 1,
					},
				), nil
			}).
			Times(4)

		computer.EXPECT().ReturnsNativeValue("type.googleapis.com/google.protobuf.UInt32Value").Return(false)
		keyState, err := recursiveComputer.GetOrCreateKeyState(
			util.Must(
				model_core.MarshalTopLevelAny(
					model_core.NewSimpleTopLevelMessage[object.LocalReference, proto.Message](
						&wrapperspb.UInt32Value{
							Value: 1,
						},
					),
				),
			),
		)
		require.NoError(t, err)

		require.NoError(
			t,
			program.RunLocal(ctx, func(ctx context.Context, siblingsGroup, dependenciesGroup program.Group) error {
				queues.ProcessAllEvaluatableKeys(dependenciesGroup, recursiveComputer)
				return recursiveComputer.WaitForEvaluation(ctx, keyState)
			}),
		)
	})

	t.Run("ComputeMissingObjectRetriesExhausted", func(t *testing.T) {
		// If the compute function keeps failing with
		// FAILED_PRECONDITION even after its consumed
		// dependencies have been invalidated and recomputed,
		// the error should be propagated instead of retrying
		// indefinitely.
		computer := NewMockComputerForTesting(ctrl)
		computer.EXPECT().IsLookup("type.googleapis.com/google.protobuf.UInt32Value").Return(false).AnyTimes()
		objectManager := NewMockObjectManagerForTesting(ctrl)
		tagStore := NewMockBoundStoreForTesting(ctrl)
		evaluationReader := NewMockProtoEvaluationReaderForTesting(ctrl)
		lookupResultReader := NewMockLookupResultReaderForTesting(ctrl)
		lookupResultReader.EXPECT().GetDecodingParametersSizeBytes().Return(16).AnyTimes()
		keysReader := NewMockKeysReaderForTesting(ctrl)
		cacheDeterministicEncoder := NewMockDeterministicBinaryEncoder(ctrl)
		cacheKeyedEncoder := NewMockKeyedBinaryEncoder(ctrl)

		queuesFactory := model_evaluation.NewSimpleRecursiveComputerEvaluationQueuesFactory[object.LocalReference, model_core.ReferenceMetadata](1)
		queues := queuesFactory.NewQueues()
		recursiveComputer := model_evaluation.NewRecursiveComputer(
			computer,
			queues,
			object.SHA256V1ReferenceFormat,
			objectManager,
			tagStore,
			/* actionTagKeyReference = */ object.MustNewSHA256V1LocalReference("5d1f8ba86bd08bf6fe4ebe96e2b83b1a8a2fd4426ef1a19881cbe10c4e6737bd", 82910, 0, 0, 0),
			evaluationReader,
			lookupResultReader,
			keysReader,
			cacheDeterministicEncoder,
			cacheKeyedEncoder,
			clock.SystemClock,
		)

		gomock.InOrder(
			tagStore.EXPECT().ResolveTag(gomock.Any(), gomock.Any()).
				Return(object.LocalReference{}, status.Error(codes.NotFound, "Tag does not exist")),
			tagStore.EXPECT().ResolveTag(gomock.Any(), gomock.Any()).
				Return(object.MustNewSHA256V1LocalReference("d78a49417cbb8a70bd8bfd6f31e93b73de9c9422e0e9d69eff8b1e4a4c58ea23", 82910, 0, 0, 0), nil),
		)
		lookupResultReader.EXPECT().ReadObject(gomock.Any(), gomock.Any()).
			Return(model_core.NewSimpleMessage[object.LocalReference](
				&model_evaluation_cache_pb.LookupResult{
					Result: &model_evaluation_cache_pb.LookupResult_HitValue{
						HitValue: &model_core_pb.Any{
							Value: util.Must(anypb.New(&wrapperspb.UInt64Value{
								Value: 7,
							})),
							References: &model_core_pb.ReferenceSet{},
						},
					},
				},
			), nil).
			Times(2)
		// Key 1 keeps failing with FAILED_PRECONDITION. The
		// first failure invalidates the cache-backed key 2, but
		// after key 2 has been recomputed its value lives in
		// memory and can no longer be invalidated, so the second
		// failure must be final. The bounded number of
		// expectations proves termination.
		computer.EXPECT().ComputeMessageValue(gomock.Any(), gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, key model_core.Message[proto.Message, object.LocalReference], e model_evaluation.Environment[object.LocalReference, model_core.ReferenceMetadata]) (model_core.PatchedMessage[proto.Message, model_core.ReferenceMetadata], error) {
				k := key.Message.(*wrapperspb.UInt32Value)
				if k.Value == 2 {
					return model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](
						&wrapperspb.UInt64Value{Value: 8},
					), nil
				}
				v := e.GetMessageValue(model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](&wrapperspb.UInt32Value{
					Value: 2,
				}))
				if !v.IsSet() {
					return model_core.PatchedMessage[proto.Message, model_core.ReferenceMetadata]{}, model_evaluation.ErrMissingDependency
				}
				return model_core.PatchedMessage[proto.Message, model_core.ReferenceMetadata]{}, status.Error(codes.FailedPrecondition, "Record not found")
			}).
			Times(4)

		computer.EXPECT().ReturnsNativeValue("type.googleapis.com/google.protobuf.UInt32Value").Return(false)
		keyState, err := recursiveComputer.GetOrCreateKeyState(
			util.Must(
				model_core.MarshalTopLevelAny(
					model_core.NewSimpleTopLevelMessage[object.LocalReference, proto.Message](
						&wrapperspb.UInt32Value{
							Value: 1,
						},
					),
				),
			),
		)
		require.NoError(t, err)

		err = program.RunLocal(ctx, func(ctx context.Context, siblingsGroup, dependenciesGroup program.Group) error {
			queues.ProcessAllEvaluatableKeys(dependenciesGroup, recursiveComputer)
			return recursiveComputer.WaitForEvaluation(ctx, keyState)
		})
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
	})

	t.Run("CacheLookupReturningUnexpectedResult", func(t *testing.T) {
		// The first cache lookup should typically contain an
		// Initial message, describing the initial set of
		// dependencies that need to be considered. If the
		// message is malformed and contains another type of
		// message, we should just ignore it.
		computer := NewMockComputerForTesting(ctrl)
		computer.EXPECT().IsLookup("type.googleapis.com/google.protobuf.UInt32Value").Return(false).AnyTimes()
		objectManager := NewMockObjectManagerForTesting(ctrl)
		tagStore := NewMockBoundStoreForTesting(ctrl)
		evaluationReader := NewMockProtoEvaluationReaderForTesting(ctrl)
		lookupResultReader := NewMockLookupResultReaderForTesting(ctrl)
		lookupResultReader.EXPECT().GetDecodingParametersSizeBytes().Return(16).AnyTimes()
		keysReader := NewMockKeysReaderForTesting(ctrl)
		cacheDeterministicEncoder := NewMockDeterministicBinaryEncoder(ctrl)
		cacheKeyedEncoder := NewMockKeyedBinaryEncoder(ctrl)

		queuesFactory := model_evaluation.NewSimpleRecursiveComputerEvaluationQueuesFactory[object.LocalReference, model_core.ReferenceMetadata](1)
		queues := queuesFactory.NewQueues()
		recursiveComputer := model_evaluation.NewRecursiveComputer(
			computer,
			queues,
			object.SHA256V1ReferenceFormat,
			objectManager,
			tagStore,
			/* actionTagKeyReference = */ object.MustNewSHA256V1LocalReference("4466f601ba900ce4e0d606dc68dd0c35c5e976792784400514f42905d6deba6e", 64722, 0, 0, 0),
			evaluationReader,
			lookupResultReader,
			keysReader,
			cacheDeterministicEncoder,
			cacheKeyedEncoder,
			clock.SystemClock,
		)

		computer.EXPECT().ReturnsNativeValue("type.googleapis.com/google.protobuf.UInt32Value").Return(false)
		keyState, err := recursiveComputer.GetOrCreateKeyState(
			util.Must(
				model_core.MarshalTopLevelAny(
					model_core.NewSimpleTopLevelMessage[object.LocalReference, proto.Message](
						&wrapperspb.UInt32Value{
							Value: 12,
						},
					),
				),
			),
		)
		require.NoError(t, err)

		tagStore.EXPECT().ResolveTag(
			gomock.Any(),
			[...]byte{
				0x57, 0x12, 0x01, 0xf0, 0x80, 0xf7, 0xe1, 0x9f,
				0x41, 0x44, 0x87, 0x88, 0x9b, 0xf3, 0xc9, 0x4f,
				0x61, 0x68, 0x71, 0xd5, 0x07, 0x06, 0x3e, 0x92,
				0xdc, 0x40, 0x42, 0xbf, 0x2a, 0x3c, 0x01, 0xec,
			},
		).Return(object.MustNewSHA256V1LocalReference("829d56c5c910ff51bb09192f5655cd51c02e54c3e16b00d2fde8a6b7e3d814fc", 95938, 0, 0, 0), nil)
		lookupResultReader.EXPECT().ReadObject(
			gomock.Any(),
			util.Must(
				model_core.NewDecodable(
					object.MustNewSHA256V1LocalReference("829d56c5c910ff51bb09192f5655cd51c02e54c3e16b00d2fde8a6b7e3d814fc", 95938, 0, 0, 0),
					[]byte{
						0x9c, 0x80, 0x78, 0x1a, 0xa4, 0x29, 0x20, 0x6d,
						0x76, 0xa4, 0x4e, 0x8f, 0x4c, 0x2e, 0xce, 0xd5,
					},
				),
			),
		).Return(model_core.NewSimpleMessage[object.LocalReference](
			&model_evaluation_cache_pb.LookupResult{
				Result: &model_evaluation_cache_pb.LookupResult_MissingDependencies_{
					MissingDependencies: &model_evaluation_cache_pb.LookupResult_MissingDependencies{},
				},
			},
		), nil)
		computer.EXPECT().ComputeMessageValue(gomock.Any(), gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, key model_core.Message[proto.Message, object.LocalReference], e model_evaluation.Environment[object.LocalReference, model_core.ReferenceMetadata]) (model_core.PatchedMessage[proto.Message, model_core.ReferenceMetadata], error) {
				return model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](
					&wrapperspb.UInt64Value{Value: 13},
				), nil
			})

		require.NoError(
			t,
			program.RunLocal(ctx, func(ctx context.Context, siblingsGroup, dependenciesGroup program.Group) error {
				queues.ProcessAllEvaluatableKeys(dependenciesGroup, recursiveComputer)
				return recursiveComputer.WaitForEvaluation(ctx, keyState)
			}),
		)
		/*
			testutil.RequireEqualProto(t, &wrapperspb.UInt64Value{
				Value: 13,
			}, value.Message)
		*/
	})

	t.Run("CacheLookupWithoutDependencies", func(t *testing.T) {
		// In the extremely rare case that a key has no
		// dependencies, the initial lookup result may a value
		// directly. There should be no need to call into the
		// computer, as the cached value can be used.
		computer := NewMockComputerForTesting(ctrl)
		computer.EXPECT().IsLookup("type.googleapis.com/google.protobuf.UInt32Value").Return(false).AnyTimes()
		objectManager := NewMockObjectManagerForTesting(ctrl)
		tagStore := NewMockBoundStoreForTesting(ctrl)
		evaluationReader := NewMockProtoEvaluationReaderForTesting(ctrl)
		lookupResultReader := NewMockLookupResultReaderForTesting(ctrl)
		lookupResultReader.EXPECT().GetDecodingParametersSizeBytes().Return(16).AnyTimes()
		keysReader := NewMockKeysReaderForTesting(ctrl)
		cacheDeterministicEncoder := NewMockDeterministicBinaryEncoder(ctrl)
		cacheKeyedEncoder := NewMockKeyedBinaryEncoder(ctrl)

		queuesFactory := model_evaluation.NewSimpleRecursiveComputerEvaluationQueuesFactory[object.LocalReference, model_core.ReferenceMetadata](1)
		queues := queuesFactory.NewQueues()
		recursiveComputer := model_evaluation.NewRecursiveComputer(
			computer,
			queues,
			object.SHA256V1ReferenceFormat,
			objectManager,
			tagStore,
			/* actionTagKeyReference = */ object.MustNewSHA256V1LocalReference("2fbd13f1ee48876ec1e85961b59877c3916fb10ae23e7e1e45083feecafe1804", 57483, 0, 0, 0),
			evaluationReader,
			lookupResultReader,
			keysReader,
			cacheDeterministicEncoder,
			cacheKeyedEncoder,
			clock.SystemClock,
		)

		computer.EXPECT().ReturnsNativeValue("type.googleapis.com/google.protobuf.UInt32Value").Return(false)
		keyState, err := recursiveComputer.GetOrCreateKeyState(
			util.Must(
				model_core.MarshalTopLevelAny(
					model_core.NewSimpleTopLevelMessage[object.LocalReference, proto.Message](
						&wrapperspb.UInt32Value{
							Value: 24,
						},
					),
				),
			),
		)
		require.NoError(t, err)

		tagStore.EXPECT().ResolveTag(
			gomock.Any(),
			[...]byte{
				0x1e, 0x0f, 0x30, 0xc1, 0x1c, 0x0a, 0x4d, 0x1c,
				0xd6, 0x18, 0x13, 0x8d, 0xa3, 0x58, 0x40, 0xfa,
				0x31, 0x22, 0x07, 0x66, 0x38, 0x45, 0x22, 0x9b,
				0x70, 0x31, 0xbb, 0xac, 0xe8, 0x70, 0x35, 0x07,
			},
		).Return(object.MustNewSHA256V1LocalReference("e1749fb7b82448ae6941d4f7e515818efbf781e6fe2738465bd9ce876b8d9866", 2345, 0, 0, 0), nil)
		lookupResultReader.EXPECT().ReadObject(
			gomock.Any(),
			util.Must(
				model_core.NewDecodable(
					object.MustNewSHA256V1LocalReference("e1749fb7b82448ae6941d4f7e515818efbf781e6fe2738465bd9ce876b8d9866", 2345, 0, 0, 0),
					[]byte{
						0xe5, 0x83, 0xfe, 0x6a, 0x73, 0xa3, 0x7f, 0xe9,
						0x5c, 0xb8, 0x4f, 0x3a, 0x54, 0xdf, 0xd3, 0x87,
					},
				),
			),
		).Return(model_core.NewSimpleMessage[object.LocalReference](
			&model_evaluation_cache_pb.LookupResult{
				Result: &model_evaluation_cache_pb.LookupResult_HitValue{
					HitValue: &model_core_pb.Any{
						Value: util.Must(anypb.New(&wrapperspb.UInt64Value{
							Value: 26,
						})),
						References: &model_core_pb.ReferenceSet{},
					},
				},
			},
		), nil)

		require.NoError(
			t,
			program.RunLocal(ctx, func(ctx context.Context, siblingsGroup, dependenciesGroup program.Group) error {
				queues.ProcessAllEvaluatableKeys(dependenciesGroup, recursiveComputer)
				return recursiveComputer.WaitForEvaluation(ctx, keyState)
			}),
		)
		/*
			testutil.RequireEqualProto(t, &wrapperspb.UInt64Value{
				Value: 26,
			}, value.Message)
		*/
	})

	t.Run("ConcurrentUpload", func(t *testing.T) {
		// Uploading the results of a key requires graphlets to
		// be constructed for its nested dependencies. Multiple
		// goroutines may upload keys whose sets of nested
		// dependencies overlap, meaning that value states of
		// shared dependencies are transitioned concurrently.
		// This used to be performed without locking, causing
		// crashes. Compute a Fibonacci sequence with an
		// overridden base case, so that all keys become
		// variable dependencies of each other, and upload the
		// results with a high amount of concurrency.
		computer := NewMockComputerForTesting(ctrl)
		computer.EXPECT().ComputeMessageValue(gomock.Any(), gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, key model_core.Message[proto.Message, object.LocalReference], e model_evaluation.Environment[object.LocalReference, model_core.ReferenceMetadata]) (model_core.PatchedMessage[proto.Message, model_core.ReferenceMetadata], error) {
				// Base case: fib(1). fib(0) is overridden
				// below, meaning it is never computed.
				k := key.Message.(*wrapperspb.UInt32Value)
				if k.Value <= 1 {
					return model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](
						&wrapperspb.UInt64Value{
							Value: uint64(k.Value),
						},
					), nil
				}

				// Recursion: fib(n) = fib(n-2) + fib(n-1).
				v0 := e.GetMessageValue(model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](&wrapperspb.UInt32Value{
					Value: k.Value - 2,
				}))
				v1 := e.GetMessageValue(model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](&wrapperspb.UInt32Value{
					Value: k.Value - 1,
				}))
				if !v0.IsSet() || !v1.IsSet() {
					return model_core.PatchedMessage[proto.Message, model_core.ReferenceMetadata]{}, model_evaluation.ErrMissingDependency
				}
				return model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](
					&wrapperspb.UInt64Value{
						Value: v0.Message.(*wrapperspb.UInt64Value).Value + v1.Message.(*wrapperspb.UInt64Value).Value,
					},
				), nil
			}).
			AnyTimes()
		computer.EXPECT().IsLookup("type.googleapis.com/google.protobuf.UInt32Value").Return(false).AnyTimes()
		computer.EXPECT().ReturnsNativeValue("type.googleapis.com/google.protobuf.UInt32Value").Return(false).AnyTimes()
		objectManager := NewMockObjectManagerForTesting(ctrl)
		objectManager.EXPECT().CaptureCreatedObject(gomock.Any(), gomock.Any()).
			Return(model_core.NoopReferenceMetadata{}, nil).
			AnyTimes()
		objectManager.EXPECT().CaptureExistingObject(gomock.Any()).
			Return(model_core.NoopReferenceMetadata{}).
			AnyTimes()
		objectManager.EXPECT().ReferenceObject(gomock.Any()).
			DoAndReturn(func(metadataEntry model_core.MetadataEntry[model_core.ReferenceMetadata]) object.LocalReference {
				return metadataEntry.LocalReference
			}).
			AnyTimes()
		tagStore := NewMockBoundStoreForTesting(ctrl)
		tagStore.EXPECT().ResolveTag(gomock.Any(), gomock.Any()).Return(object.LocalReference{}, status.Error(codes.NotFound, "Tag does not exist")).AnyTimes()
		tagStore.EXPECT().UpdateTag(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		evaluationReader := NewMockProtoEvaluationReaderForTesting(ctrl)
		evaluationReader.EXPECT().ReadObject(gomock.Any(), gomock.Any()).
			Return(model_core.NewSimpleMessage[object.LocalReference](
				&model_evaluation_pb.Evaluation{
					Value: &model_core_pb.Any{
						Value: util.Must(anypb.New(&wrapperspb.UInt64Value{
							Value: 1,
						})),
						References: &model_core_pb.ReferenceSet{},
					},
				},
			), nil).
			AnyTimes()
		lookupResultReader := NewMockLookupResultReaderForTesting(ctrl)
		lookupResultReader.EXPECT().GetDecodingParametersSizeBytes().Return(16).AnyTimes()
		lookupResultReader.EXPECT().ReadObject(gomock.Any(), gomock.Any()).
			Return(model_core.NewSimpleMessage[object.LocalReference](
				&model_evaluation_cache_pb.LookupResult{
					Result: &model_evaluation_cache_pb.LookupResult_HitGraphlet{
						HitGraphlet: &model_evaluation_pb.Graphlet{
							Evaluation: &model_evaluation_pb.Graphlet_EvaluationInline{
								EvaluationInline: &model_evaluation_pb.Evaluation{
									Value: &model_core_pb.Any{
										Value: util.Must(anypb.New(&wrapperspb.UInt64Value{
											Value: 1,
										})),
										References: &model_core_pb.ReferenceSet{},
									},
								},
							},
						},
					},
				},
			), nil).
			AnyTimes()
		keysReader := NewMockKeysReaderForTesting(ctrl)
		cacheDeterministicEncoder := model_encoding.NewChainedDeterministicBinaryEncoder(nil)
		cacheKeyedEncoder := NewMockKeyedBinaryEncoder(ctrl)
		cacheKeyedEncoder.EXPECT().EncodeBinary(gomock.Any(), gomock.Any()).
			DoAndReturn(func(in, parameters []byte) ([]byte, error) {
				return in, nil
			}).
			AnyTimes()

		queuesFactory := model_evaluation.NewSimpleRecursiveComputerEvaluationQueuesFactory[object.LocalReference, model_core.ReferenceMetadata](1)
		queues := queuesFactory.NewQueues()
		recursiveComputer := model_evaluation.NewRecursiveComputer(
			computer,
			queues,
			object.SHA256V1ReferenceFormat,
			objectManager,
			tagStore,
			/* actionTagKeyReference = */ object.MustNewSHA256V1LocalReference("8f19b2b6dd526e00c19a6f8f108451b6c00693b425b0e3bf6bcdca758b788b95", 7483, 0, 0, 0),
			evaluationReader,
			lookupResultReader,
			keysReader,
			cacheDeterministicEncoder,
			cacheKeyedEncoder,
			clock.SystemClock,
		)

		// Override fib(0), so that all other keys transitively
		// depend on an injected value and thereby become
		// variable dependencies for which graphlets need to be
		// constructed.
		require.NoError(t, recursiveComputer.OverrideKeyState(
			util.Must(model_core.ComputeTopLevelMessageReference(
				util.Must(
					model_core.MarshalTopLevelAny(
						model_core.NewSimpleTopLevelMessage[object.LocalReference, proto.Message](
							&wrapperspb.UInt32Value{
								Value: 0,
							},
						),
					),
				),
				object.SHA256V1ReferenceFormat,
			)),
			util.Must(
				model_core.MarshalTopLevelAny(
					model_core.NewSimpleTopLevelMessage[object.LocalReference, proto.Message](
						&wrapperspb.UInt64Value{
							Value: 0,
						},
					),
				),
			),
		))

		keyState, err := recursiveComputer.GetOrCreateKeyState(
			util.Must(
				model_core.MarshalTopLevelAny(
					model_core.NewSimpleTopLevelMessage[object.LocalReference, proto.Message](
						&wrapperspb.UInt32Value{
							Value: 60,
						},
					),
				),
			),
		)
		require.NoError(t, err)

		require.NoError(
			t,
			program.RunLocal(ctx, func(ctx context.Context, siblingsGroup, dependenciesGroup program.Group) error {
				queues.ProcessAllEvaluatableKeys(dependenciesGroup, recursiveComputer)
				return recursiveComputer.WaitForEvaluation(ctx, keyState)
			}),
		)

		// Upload the results of all keys with a high amount of
		// concurrency, so that value states of shared nested
		// dependencies are transitioned by multiple goroutines.
		recursiveComputer.GracefullyStopUploading()
		group, groupCtx := errgroup.WithContext(ctx)
		for i := 0; i < 8; i++ {
			group.Go(func() error {
				for {
					if shouldContinue, err := recursiveComputer.ProcessNextUploadableKey(groupCtx); err != nil || !shouldContinue {
						return err
					}
				}
			})
		}
		require.NoError(t, group.Wait())

		_, err = recursiveComputer.GetEvaluations(
			ctx,
			[]*model_evaluation.KeyState[object.LocalReference, model_core.ReferenceMetadata]{
				keyState,
			},
		)
		require.NoError(t, err)
	})

	t.Run("CacheLookupFoo", func(t *testing.T) {
		computer := NewMockComputerForTesting(ctrl)
		computer.EXPECT().IsLookup("type.googleapis.com/google.protobuf.UInt32Value").Return(false).AnyTimes()
		objectManager := NewMockObjectManagerForTesting(ctrl)
		tagStore := NewMockBoundStoreForTesting(ctrl)
		evaluationReader := NewMockProtoEvaluationReaderForTesting(ctrl)
		lookupResultReader := NewMockLookupResultReaderForTesting(ctrl)
		lookupResultReader.EXPECT().GetDecodingParametersSizeBytes().Return(16).AnyTimes()
		keysReader := NewMockKeysReaderForTesting(ctrl)
		cacheDeterministicEncoder := NewMockDeterministicBinaryEncoder(ctrl)
		cacheKeyedEncoder := NewMockKeyedBinaryEncoder(ctrl)

		queuesFactory := model_evaluation.NewSimpleRecursiveComputerEvaluationQueuesFactory[object.LocalReference, model_core.ReferenceMetadata](1)
		queues := queuesFactory.NewQueues()
		recursiveComputer := model_evaluation.NewRecursiveComputer(
			computer,
			queues,
			object.SHA256V1ReferenceFormat,
			objectManager,
			tagStore,
			/* actionTagKeyReference = */ object.MustNewSHA256V1LocalReference("bf1b2cdd5f58461827eb9285ee37dd45fea02ebc4e278f28678ea2722f590337", 57483, 0, 0, 0),
			evaluationReader,
			lookupResultReader,
			keysReader,
			cacheDeterministicEncoder,
			cacheKeyedEncoder,
			clock.SystemClock,
		)

		computer.EXPECT().ReturnsNativeValue("type.googleapis.com/google.protobuf.UInt32Value").Return(false)
		keyState, err := recursiveComputer.GetOrCreateKeyState(
			util.Must(
				model_core.MarshalTopLevelAny(
					model_core.NewSimpleTopLevelMessage[object.LocalReference, proto.Message](
						&wrapperspb.UInt32Value{
							Value: 18,
						},
					),
				),
			),
		)
		require.NoError(t, err)

		// Initial lookup for &wrapperspb.UInt32Value{Value: 18}.
		tagStore.EXPECT().ResolveTag(
			gomock.Any(),
			[...]byte{
				0xba, 0x4e, 0xe4, 0x47, 0x51, 0x09, 0x77, 0x2d,
				0x0f, 0x8f, 0x90, 0x9d, 0xce, 0xc6, 0xe2, 0x14,
				0x6a, 0xee, 0xed, 0x9c, 0x1d, 0x44, 0x9d, 0x3f,
				0xd7, 0x49, 0x74, 0xf1, 0x93, 0xad, 0xb0, 0x52,
			},
		).Return(object.MustNewSHA256V1LocalReference("cd897b5bbc720746ba0740100714e8f98f36e3c3cda42b3d3e8c0c26884c0780", 43984, 0, 0, 0), nil)
		lookupResultReader.EXPECT().ReadObject(
			gomock.Any(),
			util.Must(
				model_core.NewDecodable(
					object.MustNewSHA256V1LocalReference("cd897b5bbc720746ba0740100714e8f98f36e3c3cda42b3d3e8c0c26884c0780", 43984, 0, 0, 0),
					[]byte{
						0xf9, 0x5c, 0x1b, 0x38, 0xaa, 0xc0, 0x11, 0x82,
						0x6e, 0x8c, 0xda, 0x68, 0x47, 0x16, 0x03, 0x48,
					},
				),
			),
		).Return(model_core.NewSimpleMessage[object.LocalReference](
			&model_evaluation_cache_pb.LookupResult{
				Result: &model_evaluation_cache_pb.LookupResult_Initial_{
					Initial: &model_evaluation_cache_pb.LookupResult_Initial{
						GraphletVariableDependencyKeys: []*model_evaluation_pb.Keys{{
							Level: &model_evaluation_pb.Keys_Leaf{
								Leaf: &model_core_pb.Any{
									Value: util.Must(anypb.New(&wrapperspb.UInt32Value{
										Value: 16,
									})),
									References: &model_core_pb.ReferenceSet{},
								},
							},
						}},
						ValueVariableDependencyKeys: []*model_evaluation_pb.Keys{
							{
								Level: &model_evaluation_pb.Keys_Leaf{
									Leaf: &model_core_pb.Any{
										Value: util.Must(anypb.New(&wrapperspb.UInt32Value{
											Value: 16,
										})),
										References: &model_core_pb.ReferenceSet{},
									},
								},
							},
							{
								Level: &model_evaluation_pb.Keys_Leaf{
									Leaf: &model_core_pb.Any{
										Value: util.Must(anypb.New(&wrapperspb.UInt32Value{
											Value: 17,
										})),
										References: &model_core_pb.ReferenceSet{},
									},
								},
							},
						},
					},
				},
			},
		), nil)
		computer.EXPECT().ReturnsNativeValue("type.googleapis.com/google.protobuf.UInt32Value").Return(false)

		// Initial lookup for &wrapperspb.UInt32Value{Value: 16}.
		tagStore.EXPECT().ResolveTag(
			gomock.Any(),
			[...]byte{
				0x5e, 0x5b, 0x0e, 0xc1, 0x14, 0x87, 0x2d, 0x85,
				0x6c, 0xdc, 0x77, 0xfb, 0x44, 0x11, 0xb0, 0x8e,
				0xcd, 0xe6, 0x1e, 0x02, 0x04, 0x75, 0xeb, 0xc3,
				0xe0, 0x4a, 0x14, 0xfd, 0x17, 0xee, 0x87, 0x06,
			},
		).Return(object.LocalReference{}, status.Error(codes.NotFound, "Tag does not exist")).AnyTimes()
		computer.EXPECT().ComputeMessageValue(gomock.Any(), gomock.Any(), gomock.Any()).
			DoAndReturn(func(ctx context.Context, key model_core.Message[proto.Message, object.LocalReference], e model_evaluation.Environment[object.LocalReference, model_core.ReferenceMetadata]) (model_core.PatchedMessage[proto.Message, model_core.ReferenceMetadata], error) {
				testutil.RequireEqualProto(t, &wrapperspb.UInt32Value{
					Value: 16,
				}, key.Message)
				return model_core.NewSimplePatchedMessage[model_core.ReferenceMetadata, proto.Message](
					&wrapperspb.UInt32Value{
						Value: 987,
					},
				), nil
			})

		// Subsequent lookup for &wrapperspb.UInt32Value{Value: 18}.
		tagStore.EXPECT().ResolveTag(
			gomock.Any(),
			[...]byte{
				0xba, 0x4e, 0xe4, 0x47, 0x51, 0x09, 0x77, 0x2d,
				0x0f, 0x8f, 0x90, 0x9d, 0xce, 0xc6, 0xe2, 0x14,
				0x6a, 0xee, 0xed, 0x9c, 0x1d, 0x44, 0x9d, 0x3f,
				0xd7, 0x49, 0x74, 0xf1, 0x93, 0xad, 0xb0, 0x52,
			},
		).Return(object.MustNewSHA256V1LocalReference("423b895a5498a677b5075170e6da337499e7f69646d50adabe4ba2026ecee247", 84733, 0, 0, 0), nil)
		lookupResultReader.EXPECT().ReadObject(
			gomock.Any(),
			util.Must(
				model_core.NewDecodable(
					object.MustNewSHA256V1LocalReference("423b895a5498a677b5075170e6da337499e7f69646d50adabe4ba2026ecee247", 84733, 0, 0, 0),
					[]byte{
						0xf9, 0x5c, 0x1b, 0x38, 0xaa, 0xc0, 0x11, 0x82,
						0x6e, 0x8c, 0xda, 0x68, 0x47, 0x16, 0x03, 0x48,
					},
				),
			),
		).Return(model_core.NewSimpleMessage[object.LocalReference](
			&model_evaluation_cache_pb.LookupResult{
				Result: &model_evaluation_cache_pb.LookupResult_HitValue{
					HitValue: &model_core_pb.Any{
						Value: util.Must(anypb.New(&wrapperspb.UInt64Value{
							Value: 2584,
						})),
						References: &model_core_pb.ReferenceSet{},
					},
				},
			},
		), nil)

		require.NoError(
			t,
			program.RunLocal(ctx, func(ctx context.Context, siblingsGroup, dependenciesGroup program.Group) error {
				queues.ProcessAllEvaluatableKeys(dependenciesGroup, recursiveComputer)
				return recursiveComputer.WaitForEvaluation(ctx, keyState)
			}),
		)
		/*
			testutil.RequireEqualProto(t, &wrapperspb.UInt64Value{
				Value: 26,
			}, value.Message)
		*/
	})
}
