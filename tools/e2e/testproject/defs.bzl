# Exercises the Starlark builtins added in the "Close several Starlark
# API compatibility gaps with Bazel" change: attr.int_list(),
# native.repo_name(), native.repository_name(), native.module_name(),
# native.module_version(), native.package_name(), and
# Label.workspace_name.

def _info_file_impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name + ".txt")
    total = 0
    for n in ctx.attr.numbers:
        total += n
    ctx.actions.write(
        output = out,
        content = "%s\nsum(%s) = %d\n" % (ctx.attr.build_info, ctx.attr.numbers, total),
    )
    return [DefaultInfo(files = depset([out]))]

info_file = rule(
    implementation = _info_file_impl,
    attrs = {
        "build_info": attr.string(),
        # attr.int_list() is one of the newly added builtins.
        "numbers": attr.int_list(default = [1, 2, 3]),
    },
)

def repo_info(name, **kwargs):
    # All of the functions below run at macro expansion time and are
    # newly added builtins.
    info_file(
        name = name,
        build_info = "module=%s version=%s repo=%r repository_name=%r package=%r workspace_name=%r" % (
            native.module_name(),
            native.module_version(),
            native.repo_name(),
            native.repository_name(),
            native.package_name(),
            Label("//:BUILD.bazel").workspace_name,
        ),
        **kwargs
    )
