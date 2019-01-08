all: build

build: build-mysqlbin

build-mysqlbin:
	go build -o bin/go-mysqlbin ./cmd/go-mysqlbin

test:
	go test -timeout 1m --race ./...

clean:
	go clean -i ./...
	@rm -rf bin


update_vendor:
	which glide >/dev/null || curl https://glide.sh/get | sh
	which glide-vc || go get -v -u github.com/sgotti/glide-vc
	glide update --strip-vendor
	@echo "removing test files"
	glide-vc --only-code --no-tests --use-lock-file