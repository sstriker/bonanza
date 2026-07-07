"""Aspect, provider and depset conformance checks.

A collector aspect walks the diamond graph below along "deps", merging
transitive depsets of labels. The aspect_reader rule consumes the
provider computed by the aspect and materializes it, so that the verify
genrule can assert on aspect propagation, provider merging, and depset
deduplication/ordering.

Additional rules and aspects assert propagation across
attr.string_keyed_label_dict() attributes, filtering based on
required_providers against providers advertised through
rule(provides = ...), aspect composition through requires and
required_aspect_providers, materialization of an aspect's own
attrs (a configured private label attr and a string parameter)
through ctx.attr, resolution of aspect(toolchains = ...) into
ctx.toolchains (both a registered mandatory toolchain and an
optional toolchain type without a registered implementation), and
observation of a target's registered actions through target.actions
(mnemonics of synthesized actions, the declared types of their
output Files, and the content of files created through
ctx.actions.write() and expand_template()).
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
    fields = ["labels", "attr_info"],
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
    # The aspect's own attrs are configured from their default values
    # and exposed through ctx.attr, separate from the attrs of the
    # visited rule target exposed through ctx.rule.attr. The private
    # "_marker" label attr must have been configured into a Target
    # carrying its providers, and its default label must have been
    # resolved relative to the package declaring this aspect.
    if NodeInfo not in ctx.attr._marker:
        fail("ctx.attr._marker does not provide NodeInfo")
    attr_info = "marker={} fmt={}".format(
        ctx.attr._marker[NodeInfo].label_str,
        ctx.attr.fmt,
    )
    deps = list(getattr(ctx.rule.attr, "deps", []))
    deps += getattr(ctx.rule.attr, "tools", {}).values()
    transitive = [dep[TransitiveInfo].labels for dep in deps]
    labels = depset(
        direct = [str(target.label)],
        transitive = transitive,
        order = "postorder",
    )
    return [TransitiveInfo(labels = labels, attr_info = attr_info)]

collector_aspect = aspect(
    implementation = _collector_aspect_impl,
    attr_aspects = [
        "deps",
        "tools",
    ],
    # Declared providers must be returned by the implementation
    # function; a regression in either the encoding or the
    # enforcement of provides fails the build.
    provides = [TransitiveInfo],
    attrs = {
        "_marker": attr.label(
            default = ":leaf",
            providers = [NodeInfo],
        ),
        "fmt": attr.string(
            default = "plain",
            values = ["plain", "fancy"],
        ),
    },
)

def _aspect_reader_impl(ctx):
    lines = []
    for dep in ctx.attr.deps:
        info = dep[TransitiveInfo]
        labels = info.labels.to_list()
        lines.append("{}: [{}]".format(str(dep.label), ", ".join(labels)))
        lines.append("{} {}".format(str(dep.label), info.attr_info))
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

ToolchainProbeInfo = provider(
    doc = "Computed by toolchain_probe_aspect from ctx.toolchains.",
    fields = ["text"],
)

# A trivial toolchain implementation whose ToolchainInfo carries a
# recognizable value, so that toolchain_probe_aspect can prove that
# it observed the registered toolchain through ctx.toolchains.
def _probe_toolchain_impl(ctx):
    return [platform_common.ToolchainInfo(value = ctx.attr.value)]

probe_toolchain = rule(
    implementation = _probe_toolchain_impl,
    attrs = {
        "value": attr.string(mandatory = True),
    },
    provides = [platform_common.ToolchainInfo],
)

# The aspect's toolchains are resolved against the configuration of
# the target it is applied to. The mandatory toolchain type has a
# registered implementation, while the optional one does not and must
# therefore yield None.
def _toolchain_probe_aspect_impl(target, ctx):
    return [ToolchainProbeInfo(text = "resolved={} optional={}".format(
        ctx.toolchains["//:aspect_toolchain_type"].value,
        ctx.toolchains["//:optional_toolchain_type"] == None,
    ))]

toolchain_probe_aspect = aspect(
    implementation = _toolchain_probe_aspect_impl,
    provides = [ToolchainProbeInfo],
    toolchains = [
        "//:aspect_toolchain_type",
        config_common.toolchain_type("//:optional_toolchain_type", mandatory = False),
    ],
)

def _toolchain_probe_reader_impl(ctx):
    lines = [dep[ToolchainProbeInfo].text for dep in ctx.attr.deps]
    out = ctx.actions.declare_file(ctx.label.name + ".txt")
    ctx.actions.write(out, "\n".join(lines) + "\n")
    return [DefaultInfo(files = depset([out]))]

toolchain_probe_reader = rule(
    implementation = _toolchain_probe_reader_impl,
    attrs = {
        "deps": attr.label_list(aspects = [toolchain_probe_aspect]),
    },
)

ActionEdgesInfo = provider(
    doc = "Computed by action_edges_aspect from target.actions.",
    fields = ["lines"],
)

# A rule that registers one output of each flavor that is not backed
# by a command: ctx.actions.write() with both string and Args content,
# ctx.actions.expand_template(), both flavors of ctx.actions.symlink(),
# and a directory output produced by ctx.actions.run(), so that
# action_edges_aspect can observe the synthesized Action structs.
def _action_edges_node_impl(ctx):
    out_write = ctx.actions.declare_file(ctx.label.name + ".write.txt")
    ctx.actions.write(out_write, "hello\n")

    out_expand = ctx.actions.declare_file(ctx.label.name + ".expand.txt")
    ctx.actions.expand_template(
        template = ctx.file._template,
        output = out_expand,
        substitutions = {
            "{GREETING}": "hello",
            "{NAME}": "conformance",
        },
    )

    # Args rendered in the default "shell" parameter file format.
    args_shell = ctx.actions.args()
    args_shell.add("--foo")
    args_shell.add("hello world")
    out_args = ctx.actions.declare_file(ctx.label.name + ".args.txt")
    ctx.actions.write(out_args, args_shell)

    # set_param_file_format() must be honored even though
    # use_param_file() is never called.
    args_multiline = ctx.actions.args()
    args_multiline.set_param_file_format("multiline")
    args_multiline.add("a b")
    args_multiline.add("c")
    out_multiline = ctx.actions.declare_file(ctx.label.name + ".multi.txt")
    ctx.actions.write(out_multiline, args_multiline)

    out_dir = ctx.actions.declare_directory(ctx.label.name + ".dir")
    ctx.actions.run(
        executable = "/bin/mkdir",
        arguments = ["-p", out_dir.path],
        outputs = [out_dir],
        mnemonic = "MakeDirectory",
    )

    # Args referencing a directory cannot be rendered during the
    # analysis phase, so the content of this action must be None.
    args_dir = ctx.actions.args()
    args_dir.add_all([out_dir])
    out_args_dir = ctx.actions.declare_file(ctx.label.name + ".argsdir.txt")
    ctx.actions.write(out_args_dir, args_dir)

    out_symlink = ctx.actions.declare_symlink(ctx.label.name + ".sym")
    ctx.actions.symlink(
        output = out_symlink,
        target_path = "../some/target",
    )

    out_symlink_file = ctx.actions.declare_file(ctx.label.name + ".symfile")
    ctx.actions.symlink(
        output = out_symlink_file,
        target_file = out_write,
    )

    return [DefaultInfo(files = depset([out_write]))]

action_edges_node = rule(
    implementation = _action_edges_node_impl,
    attrs = {
        "_template": attr.label(
            allow_single_file = True,
            default = ":action_edges_template.txt",
        ),
    },
)

# The Action structs observed through target.actions must carry
# mnemonics matching the API through which they were registered
# (FileWrite for write(), TemplateExpand for expand_template(), and
# Symlink for both flavors of symlink()), their output Files must
# report is_directory/is_symlink as declared, and their content field
# must expose the bytes that building the output would yield for
# write() and expand_template(), or None for actions whose content
# cannot be computed during the analysis phase.
def _action_edges_aspect_impl(target, ctx):
    lines = []
    for a in target.actions:
        outs = []
        for f in a.outputs.to_list():
            mark = "F"
            if f.is_directory:
                mark = "D"
            elif f.is_symlink:
                mark = "L"
            outs.append("{}[{}]".format(f.basename, mark))
            lines.append("content {} = {}".format(f.basename, repr(a.content)))
        lines.append("{} -> {}".format(a.mnemonic, ", ".join(sorted(outs))))
    return [ActionEdgesInfo(lines = sorted(lines))]

action_edges_aspect = aspect(
    implementation = _action_edges_aspect_impl,
    provides = [ActionEdgesInfo],
)

def _action_edges_reader_impl(ctx):
    lines = []
    for dep in ctx.attr.deps:
        lines += dep[ActionEdgesInfo].lines
    out = ctx.actions.declare_file(ctx.label.name + ".txt")
    ctx.actions.write(out, "\n".join(lines) + "\n")
    return [DefaultInfo(files = depset([out]))]

action_edges_reader = rule(
    implementation = _action_edges_reader_impl,
    attrs = {
        "deps": attr.label_list(aspects = [action_edges_aspect]),
    },
)
