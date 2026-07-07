# rules_python defines enum-like structs whose methods are lambdas that
# capture the struct itself through a closure cell (see
# python/private/enum.bzl), making the resulting values recursively
# defined. Bonanza encodes the cycle by emitting a reference back to the
# enum's own global. Calling flag_values() on the decoded enum resolves
# that reference, both while this file is being compiled and while the
# probe rule below is being analyzed.

load("@rules_python//python/private:flags.bzl", "AddSrcsToRunfilesFlag")

_LOAD_TIME_VALUES = AddSrcsToRunfilesFlag.flag_values()

def _flag_enum_probe_impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name + ".txt")
    ctx.actions.write(
        output = out,
        content = "load_time=%s analysis_time=%s\n" % (
            _LOAD_TIME_VALUES,
            AddSrcsToRunfilesFlag.flag_values(),
        ),
    )
    return [DefaultInfo(files = depset([out]))]

flag_enum_probe = rule(
    implementation = _flag_enum_probe_impl,
)
