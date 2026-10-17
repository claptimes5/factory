#!/bin/sh
# Create the labels the factory uses. Needs the gh CLI, authenticated.
set -eu
repo="${1:-claptimes5/factory}"
gh label create "factory:ready"        -R "$repo" -c 0e8a16 -d "Factory: pick this up" --force
gh label create "factory:running"      -R "$repo" -c fbca04 -d "Factory: a job is running" --force
gh label create "factory:parked"       -R "$repo" -c d93f0b -d "Factory: needs a human; remove to retry" --force
gh label create "factory:awaiting-you" -R "$repo" -c 1d76db -d "Factory: draft PR ready for your review" --force
