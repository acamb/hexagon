# Hexagon base image.
#
# A session image must provide git, tmux and claude on the PATH; everything else
# is up to the image. The repository is not baked in: it arrives as a bind mount
# on /workspace, and the container runs as the host user so the files it writes
# stay owned by you. Likewise, code-server for the VS Code integration is never
# part of this image: Hexagon bind mounts one release for every session that
# asks for it, so it does not belong here either.

FROM node:22-bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      git tmux ca-certificates curl less ripgrep procps openssh-client \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g @anthropic-ai/claude-code

# Git authenticates with the token Hexagon puts in the environment, so no
# credentials are ever written to disk inside the container.
RUN printf '#!/bin/sh\ncase "$1" in *Username*) echo x-access-token ;; *) echo "$GITHUB_TOKEN" ;; esac\n' \
      > /usr/local/bin/git-askpass && chmod +x /usr/local/bin/git-askpass
ENV GIT_ASKPASS=/usr/local/bin/git-askpass

WORKDIR /workspace
CMD ["sleep", "infinity"]
