# syntax=docker/dockerfile:1.27.1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
#
# bbox-host: a reference overlay host image with the bbox module
# (tm_bbox_<office> and ls_bbox) built in, so a host is one image and needs
# no file copied onto it. Build from the repository root:
#
#   docker build -f host/Dockerfile \
#     --build-arg REFERENCE_HOST_IMAGE=<reference overlay host image> \
#     -t ghcr.io/lightwebinc/bbox-host .
#
# REFERENCE_HOST_IMAGE names the overlay host image the module is added to:
# one whose application is /app/dist/index.js with its node_modules beside
# it, that loads the absolute paths in OVERLAY_MODULES and reads the
# OVERLAY_* settings. It is a build argument with no default, so what the
# image is built on is always stated, and a release pins it; the
# placeholder below only makes a build that forgot it fail with a name that
# says so.

ARG REFERENCE_HOST_IMAGE=set-the-reference-host-image-build-arg

# The bundle is JavaScript, the same bytes on every platform, so it is
# built once on the build machine's own platform; the final stage only
# copies files onto the host image, so an arm64 image needs no emulation.
FROM --platform=$BUILDPLATFORM node:24@sha256:64af3819f9275802414d7cdc38c27e9d82bd564dec4d4da87d008255d36c63b4 AS bundle
WORKDIR /src/host
COPY host/package.json host/package-lock.json ./
COPY host/vendor ./vendor
RUN npm ci --no-audit --no-fund
COPY host/ ./
RUN npm run check && npm run build && npm run bundle

FROM ${REFERENCE_HOST_IMAGE}
# Beside the host's own node_modules, so the bundle's one import (@bsv/sdk)
# resolves to the host's copy (docs/host.md).
COPY --from=bundle /src/host/bundle/bbox-module.js /app/modules/bbox/bbox-module.js
COPY --chmod=0755 host/entrypoint.sh /usr/local/bin/bbox-host
COPY LICENSE NOTICE LICENSE-THIRD-PARTY /usr/share/doc/bbox/
# The state directory, kept for the host's life (docs/host.md): the outpoint
# rows and the payment ledger. Mount a volume here. Others may read it: the
# rows are public facts, and a payment pays only the payee's key, so the
# payee's settle can read the ledger as another user.
USER root
RUN mkdir -p /var/lib/bbox && chown node:node /var/lib/bbox && chmod 0755 /var/lib/bbox
USER node
ENV OVERLAY_MODULES=/app/modules/bbox/bbox-module.js \
    BBOX_STATE_DIR=/var/lib/bbox
ENTRYPOINT ["/usr/local/bin/bbox-host"]
