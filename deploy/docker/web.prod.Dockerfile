# syntax=docker/dockerfile:1
# Build the SPA with bun, serve the static client with nginx. The app has no
# server functions (verified: no createServerFn in web/src), so SPA mode emits
# a static client under .output/public — no Node runtime in the final image.
# Build context is web/ (this Dockerfile lives in deploy/docker/ but is built
# with `-f deploy/docker/web.prod.Dockerfile web`).
FROM oven/bun:1-alpine AS build
WORKDIR /app
COPY package.json bun.lock ./
RUN bun install --frozen-lockfile
COPY . .
RUN bun run build

FROM nginx:1.27-alpine
# TanStack Start SPA mode emits the shell as _shell.html (not index.html).
COPY --from=build /app/.output/public /usr/share/nginx/html
# nginx:alpine ships a default welcome index.html at the html root; the COPY
# above merges over it (public has no index.html), so remove it or "/" serves
# the welcome page instead of the SPA shell.
RUN rm -f /usr/share/nginx/html/index.html
# Standalone default so the image serves the SPA on its own. The Helm chart
# mounts a rendered config over this that also proxies /api,/admin,/e to the
# server; running this image alone, API calls 502 (no server), as expected.
RUN printf 'server {\n  listen 80;\n  server_name _;\n  client_max_body_size 25m;\n  root /usr/share/nginx/html;\n  index _shell.html;\n  location /assets/ { try_files $uri =404; add_header Cache-Control \"public, max-age=31536000, immutable\"; }\n  location / { try_files $uri $uri/ /_shell.html; add_header Cache-Control \"no-cache\"; }\n}\n' > /etc/nginx/conf.d/default.conf
EXPOSE 80
