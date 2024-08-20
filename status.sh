#!/usr/bin/env bash

git_sha=$(git rev-parse --short HEAD)
if [[ $? != 0 ]];
then
    exit 1
fi

echo "STABLE_GIT_SHA ${git_sha}"
echo "STABLE_VERSION v0.0.4"
