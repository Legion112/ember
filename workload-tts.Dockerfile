# An ember workload image: a node that can speak.
#
# Built on the node image, for the reason workload-sd.Dockerfile is: ember runs
# *one* container that is both the machine and the work, so it needs sshd and
# embernode alongside whatever it was rented to do.
#
# piper rather than a Python synthesiser, deliberately and for this project's
# own rule: it is a C++ binary with onnxruntime beside it, so there is no
# interpreter on the node and nothing is compiled on arrival. A runtime that
# interprets on arrival is the same mistake as compiling on a rented machine --
# bought with GPU time, reported by nothing.
#
# This is a CPU image and there is no CUDA variant, which is a measurement
# rather than an omission: piper's released binary links the CPU execution
# provider only, so a card would do nothing for it. See AGENTS.md.
ARG NODE_IMAGE=ember:lab
FROM ${NODE_IMAGE}

# Pinned to a release tag and verified by digest, as the sd image is. piper's
# repository has since been rewritten into a Python package (`piper1-gpl`), so
# this tag is not merely a pin against drift: it is the last release that ships
# a self-contained binary at all, and a moving download here would replace the
# whole argument for choosing it.
ARG PIPER_RELEASE=2023.11.14-2
ARG PIPER_ASSET=piper_linux_x86_64.tar.gz
ARG PIPER_SHA256=a50cb45f355b7af1f6d758c1b360717877ba0a398cc8cbe6d2a7a3a26e225992

ADD --checksum=sha256:${PIPER_SHA256} \
    https://github.com/rhasspy/piper/releases/download/${PIPER_RELEASE}/${PIPER_ASSET} \
    /tmp/piper.tar.gz

RUN mkdir -p /opt/piper \
 && tar xzf /tmp/piper.tar.gz -C /opt --no-same-owner \
 && rm -f /tmp/piper.tar.gz \
 && test -x /opt/piper/piper

# The shared objects live beside the binary, so the loader is told where to
# look rather than relying on an LD_LIBRARY_PATH the SSH session would not
# inherit -- the lesson workload-sd.Dockerfile records: a non-interactive
# `ssh host command` gets a very small environment, and a model that loads from
# a terminal and not from ember is a confusing afternoon.
RUN printf '/opt/piper\n' > /etc/ld.so.conf.d/ember-piper.conf && ldconfig \
 && ln -s /opt/piper/piper /usr/local/bin/piper

# espeak-ng-data is found relative to argv[0] unless it is named, and a relative
# lookup that works from /opt/piper and not from elsewhere is the same confusing
# afternoon one directory over. Named explicitly by every invocation ember
# builds; recorded here so a hand-run reproduces it.
ENV ESPEAK_DATA_PATH=/opt/piper/espeak-ng-data

# The voice is baked in, which is the opposite of the rule for a diffusion
# checkpoint and the same choice workload-sd.Dockerfile makes for sd15.
#
# AGENTS.md's measurement is that a COPY'd model gets a layer digest that is
# ours alone, so it can never hit a shared cache -- true, and it was 51% of
# ember-sd at 2.2 GB. This voice is 60 MiB, and what baking buys is that a
# local run needs no store, no mount and no sync: the whole point of the local
# provider is proving the pipeline for nothing. The digest is still checked,
# one layer earlier, by ADD --checksum.
#
# A rented card is the case where this is wrong, and the catalogue entry for one
# names the voice as an asset with the same digest instead.
ARG VOICE_ONNX_SHA256=5efe09e69902187827af646e1a6e9d269dee769f9877d17b16b1b46eeaaf019f
# The .onnx digest is confirmed twice -- Hugging Face publishes it as the LFS
# object id and a sha256sum of the download agrees -- which is the calibration
# habit internal/provider/runpod/testdata/README.md set. The .json is under the
# LFS threshold, so Hugging Face publishes no sha256 for it and the figure below
# has ONE confirmation: a sha256sum of the file served at this tag. Said rather
# than left to be assumed equal evidence.
ARG VOICE_JSON_SHA256=efe19c417bed055f2d69908248c6ba650fa135bc868b0e6abb3da181dab690a0
ARG VOICE=en_US-lessac-medium
ARG VOICE_BASE=https://huggingface.co/rhasspy/piper-voices/resolve/v1.0.0/en/en_US/lessac/medium

ADD --checksum=sha256:${VOICE_ONNX_SHA256} \
    ${VOICE_BASE}/${VOICE}.onnx /opt/voices/${VOICE}.onnx
ADD --checksum=sha256:${VOICE_JSON_SHA256} \
    ${VOICE_BASE}/${VOICE}.onnx.json /opt/voices/${VOICE}.onnx.json

RUN mkdir -p /workspace/out
