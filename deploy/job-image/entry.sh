#!/bin/sh
# Starts a checkout's gems (vendor/bundle, where jobs install them) from
# the ones its image was built with, when it has none yet, so bundle install
# only builds what a change adds: a repo's test stage often has no compiler.
g=$FACTORY_IMAGE_GEMS
if [ -n "$g" ] && [ -d "$g/ruby" ] && [ "$BUNDLE_PATH" = vendor/bundle ] && [ -f Gemfile ] && [ ! -e vendor/bundle/ruby ] && [ -w . ]; then
	mkdir -p vendor/bundle && cp -R "$g/ruby" vendor/bundle/ || rm -rf vendor/bundle/ruby
fi
exec "$@"
