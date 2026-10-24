FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /donobox .

# headless-shell ships a stripped-down Chrome for chromedp.
FROM chromedp/headless-shell:latest
# Match the host user so bind-mounted secrets can stay chmod 600.
ARG UID=1000
ARG GID=1000
RUN groupadd --gid ${GID} app \
 && useradd --uid ${UID} --gid ${GID} --create-home app \
 && mkdir -p /data /config && chown app:app /data
COPY --from=build /donobox /usr/local/bin/donobox
ENV LISTEN_ADDR=:8080 \
    DB_PATH=/data/donobox.db \
    CHROME_PROFILE_DIR=/data/chrome \
    AMAZON_COOKIES=/config/cookies.json \
    CHROME_PATH=/headless-shell/headless-shell \
    # Chrome's sandbox needs user namespaces, which Docker's default seccomp profile blocks.
    CHROME_NO_SANDBOX=1
USER app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/donobox"]
