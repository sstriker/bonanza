"""Stubs for the Windows launcher tools.

Rules like py_binary reference @bazel_tools//src/tools/launcher targets
through implicit attributes, even though the launcher is only invoked
when targeting Windows. Provide stub executables, so that analysis of
such rules succeeds on other platforms.
"""

def _stub_binary_impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name + ".sh")
    ctx.actions.write(
        out,
        "#!/bin/sh\necho 'stub for %s' >&2\nexit 1\n" % ctx.label.name,
        is_executable = True,
    )
    return [DefaultInfo(
        executable = out,
        files = depset([out]),
    )]

stub_binary = rule(
    _stub_binary_impl,
    executable = True,
)
