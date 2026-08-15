#!/usr/bin/env bash
# The pipeline registered in infra (states/aws/ci/buildkite) runs a script from
# the repository rather than uploading a file, so point it here.
set -o errexit -o nounset -o pipefail

buildkite-agent pipeline upload .buildkite/pipeline.yml
