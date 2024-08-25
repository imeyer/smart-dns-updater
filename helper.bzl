"""Helper module to create binaries per PLATFORM-ARCH combination"""

load("@rules_go//go:def.bzl", "go_binary")

# Function to create a go_binary target for each platform-arch combination
def cross_compile_binary(name, goos, goarch):
    go_binary(
        name = "{}_{}_{}".format(name, goos, goarch),
        embed = [":{}_lib".format(name)],
        goarch = goarch,
        goos = goos,
        visibility = ["//visibility:public"],
    )
