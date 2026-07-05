# Frontend dev image. This is a bun project (see bun.lock / .cta.json) — bun is
# the only supported package manager here; do not add a package-lock.json. Deps
# install at BUILD time so the container starts Vite instantly instead of running
# a multi-minute install on boot. The compose service bind-mounts web/ for hot
# reload and keeps this image's Linux node_modules via an anonymous volume (host
# macOS deps would be the wrong platform).
FROM oven/bun:1-alpine
WORKDIR /app
COPY package.json bun.lock ./
RUN bun install
COPY . .
EXPOSE 3000
# vite's dev script already sets --port 3000; add --host so it binds outside the
# container. Extra args after the script name are forwarded to it by bun.
CMD ["bun", "run", "dev", "--host", "0.0.0.0"]
