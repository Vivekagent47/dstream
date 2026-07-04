# Frontend dev image. Installs node_modules at BUILD time (visible in
# `docker compose build` output) so the container starts Vite instantly instead
# of running a silent multi-minute `npm install` on every boot. The compose
# service bind-mounts web/ for hot reload and keeps this image's node_modules
# via an anonymous volume (host macOS deps would be the wrong platform).
FROM node:22-alpine
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
EXPOSE 3000
CMD ["npm", "run", "dev", "--", "--host", "0.0.0.0"]
