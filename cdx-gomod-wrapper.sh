#!/usr/bin/env bash

env

echo "$@"

cyclonedx-gomod $@
