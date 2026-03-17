# Build
go build -o bin/docker-pull ./cmd/docker-pull/

# Pull an image
#./docker-pull nginx:latest

# With proxy
#./docker-pull --proxy socks5://127.0.0.1:1080 nginx:latest

# Resume interrupted download
#./docker-pull --resume tmp_nginx_latest

# Cross-platform: just set GOOS/GOARCH
#GOOS=windows GOARCH=amd64 go build -o docker-pull.exe ./cmd/docker-pull/

