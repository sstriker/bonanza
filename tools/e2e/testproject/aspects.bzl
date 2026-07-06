"""Aspect, provider and depset conformance checks.

A collector aspect walks the diamond graph below along "deps", merging
transitive depsets of labels. The aspect_reader rule consumes the
provider computed by the aspect and materializes it, so that the verify
genrule can assert on aspect propagation, provider merging, and depset
deduplication/ordering.
"""

load("@bazel_skylib//lib:partial.bzl", "partial")
load("@bazel_skylib//lib:sets.bzl", "sets")

def _add(x, y):
    return x + y

def _self_check():
    # Loading-phase checks of bazel_skylib helpers.
    s = sets.make([1, 2, 2, 3])
    if sets.length(s) != 3:
        fail("sets.make() did not deduplicate: %s" % sets.str(s))
    if partial.call(partial.make(_add, 2), 3) != 5:
        fail("partial.make()/call() misbehaved")

_self_check()

NodeInfo = provider(
    doc = "Marker provider for the node() rule.",
    fields = ["label_str"],
)

TransitiveInfo = provider(
    doc = "Computed by collector_aspect while walking deps.",
    fields = ["labels"],
)

def _node_impl(ctx):
    return [NodeInfo(label_str = str(ctx.label))]

node = rule(
    implementation = _node_impl,
    attrs = {
        "deps": attr.label_list(providers = [NodeInfo]),
    },
)

def _collector_aspect_impl(target, ctx):
    transitive = [dep[TransitiveInfo].labels for dep in ctx.rule.attr.deps]
    labels = depset(
        direct = [str(target.label)],
        transitive = transitive,
        order = "postorder",
    )
    return [TransitiveInfo(labels = labels)]

collector_aspect = aspect(
    implementation = _collector_aspect_impl,
    attr_aspects = ["deps"],
)

def _aspect_reader_impl(ctx):
    lines = []
    for dep in ctx.attr.deps:
        labels = dep[TransitiveInfo].labels.to_list()
        lines.append("{}: [{}]".format(str(dep.label), ", ".join(labels)))
    out = ctx.actions.declare_file(ctx.label.name + ".txt")
    ctx.actions.write(out, "\n".join(lines) + "\n")
    return [DefaultInfo(files = depset([out]))]

aspect_reader = rule(
    implementation = _aspect_reader_impl,
    attrs = {
        "deps": attr.label_list(aspects = [collector_aspect]),
    },
)
