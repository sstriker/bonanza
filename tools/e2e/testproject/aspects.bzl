"""Aspect, provider and depset conformance checks.

A collector aspect walks the diamond graph below along "deps", merging
transitive depsets of labels. The aspect_reader rule consumes the
provider computed by the aspect and materializes it, so that the verify
genrule can assert on aspect propagation, provider merging, and depset
deduplication/ordering.

Additional rules and aspects assert propagation across
attr.string_keyed_label_dict() attributes, filtering based on
required_providers against providers advertised through
rule(provides = ...), and aspect composition through requires and
required_aspect_providers.
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
    deps = list(getattr(ctx.rule.attr, "deps", []))
    deps += getattr(ctx.rule.attr, "tools", {}).values()
    transitive = [dep[TransitiveInfo].labels for dep in deps]
    labels = depset(
        direct = [str(target.label)],
        transitive = transitive,
        order = "postorder",
    )
    return [TransitiveInfo(labels = labels)]

collector_aspect = aspect(
    implementation = _collector_aspect_impl,
    attr_aspects = [
        "deps",
        "tools",
    ],
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

# A rule whose only dependency edges are the values of an
# attr.string_keyed_label_dict() attribute, so that aspect propagation
# across dict valued attributes can be asserted.
dict_node = rule(
    implementation = _node_impl,
    attrs = {
        "tools": attr.string_keyed_label_dict(),
    },
)

def _dict_reader_impl(ctx):
    lines = []
    for key in sorted(ctx.attr.tools.keys()):
        dep = ctx.attr.tools[key]
        labels = dep[TransitiveInfo].labels.to_list()
        lines.append("{} -> {}: [{}]".format(key, str(dep.label), ", ".join(labels)))
    out = ctx.actions.declare_file(ctx.label.name + ".txt")
    ctx.actions.write(out, "\n".join(lines) + "\n")
    return [DefaultInfo(files = depset([out]))]

dict_reader = rule(
    implementation = _dict_reader_impl,
    attrs = {
        "tools": attr.string_keyed_label_dict(aspects = [collector_aspect]),
    },
)

MarkerInfo = provider(
    doc = "Advertised by marked_node via rule(provides = ...).",
    fields = ["mark"],
)

MarkedInfo = provider(
    doc = "Computed by marked_collector_aspect.",
    fields = ["labels"],
)

def _marked_node_impl(ctx):
    return [
        NodeInfo(label_str = str(ctx.label)),
        MarkerInfo(mark = ctx.label.name),
    ]

marked_node = rule(
    implementation = _marked_node_impl,
    attrs = {
        "deps": attr.label_list(),
    },
    provides = [MarkerInfo, NodeInfo],
)

# As this aspect has required_providers, it must only be applied to
# (and propagate through) targets whose rule advertises MarkerInfo.
# Other targets are skipped silently.
def _marked_collector_aspect_impl(target, ctx):
    transitive = [
        dep[MarkedInfo].labels
        for dep in ctx.rule.attr.deps
        if MarkedInfo in dep
    ]
    labels = depset(
        direct = [str(target.label)],
        transitive = transitive,
        order = "postorder",
    )
    return [MarkedInfo(labels = labels)]

marked_collector_aspect = aspect(
    implementation = _marked_collector_aspect_impl,
    attr_aspects = ["deps"],
    required_providers = [MarkerInfo],
    provides = [MarkedInfo],
)

def _marked_reader_impl(ctx):
    lines = []
    for dep in ctx.attr.deps:
        labels = dep[MarkedInfo].labels.to_list()
        lines.append("{}: [{}]".format(str(dep.label), ", ".join(labels)))
    out = ctx.actions.declare_file(ctx.label.name + ".txt")
    ctx.actions.write(out, "\n".join(lines) + "\n")
    return [DefaultInfo(files = depset([out]))]

marked_reader = rule(
    implementation = _marked_reader_impl,
    attrs = {
        "deps": attr.label_list(aspects = [marked_collector_aspect]),
    },
)

AInfo = provider(
    doc = "Computed by a_aspect and advertised through provides.",
    fields = ["value"],
)

BInfo = provider(
    doc = "Computed by b_aspect, which requires a_aspect.",
    fields = ["text"],
)

CInfo = provider(
    doc = "Computed by c_aspect, which requires b_aspect.",
    fields = ["text"],
)

def _a_aspect_impl(target, ctx):
    return [AInfo(value = "A(" + str(target.label) + ")")]

a_aspect = aspect(
    implementation = _a_aspect_impl,
    attr_aspects = ["deps"],
    provides = [AInfo],
)

# b_aspect requires a_aspect directly, so a_aspect is applied to the
# same targets first and its providers are visible both on the target
# argument and on the target's dependencies.
def _b_aspect_impl(target, ctx):
    dep_values = [
        dep[AInfo].value
        for dep in ctx.rule.attr.deps
        if AInfo in dep
    ]
    return [BInfo(text = "{} deps=[{}]".format(target[AInfo].value, ", ".join(dep_values)))]

b_aspect = aspect(
    implementation = _b_aspect_impl,
    attr_aspects = ["deps"],
    requires = [a_aspect],
    required_aspect_providers = [AInfo],
)

# c_aspect only requires b_aspect directly. BInfo is visible because
# b_aspect is directly required, while AInfo is visible because the
# transitively required a_aspect advertises providers satisfying
# required_aspect_providers.
def _c_aspect_impl(target, ctx):
    return [CInfo(text = "C({} | {})".format(target[AInfo].value, target[BInfo].text))]

c_aspect = aspect(
    implementation = _c_aspect_impl,
    attr_aspects = ["deps"],
    requires = [b_aspect],
    required_aspect_providers = [AInfo],
)

def _composed_reader_impl(ctx):
    lines = [dep[CInfo].text for dep in ctx.attr.deps]
    out = ctx.actions.declare_file(ctx.label.name + ".txt")
    ctx.actions.write(out, "\n".join(lines) + "\n")
    return [DefaultInfo(files = depset([out]))]

composed_reader = rule(
    implementation = _composed_reader_impl,
    attrs = {
        "deps": attr.label_list(aspects = [c_aspect]),
    },
)
