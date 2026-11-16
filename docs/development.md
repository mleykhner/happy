# Разработка Happy

[← README](../README.md) · [Интеграционные тесты WireGuard](../integration/README.md)

## Локальная сборка

Нужны Go ≥1.24, Node.js ≥22.12, npm, curl, unzip и shasum. Для реальных сетевых тестов нужны разрешенные локальные TCP/UDP-сокеты.

```sh
tar -xzf packaging/vendor.tar.gz
cd web
npm ci --ignore-scripts
npm run build
cd ..
CGO_ENABLED=0 go build -mod=vendor -o bin/happy ./cmd/happy
sh scripts/fetch-xray.sh darwin-arm64 ./bin  # на Linux: amd64 или arm64
HAPPY_XRAY=$PWD/bin/xray HAPPY_PASSWORD_FILE=/absolute/path/password ./bin/happy serve
```

Встроенная панель собирается в `internal/webui/dist`. Для Vite dev server используйте `HAPPY_ORIGIN=http://127.0.0.1:5173`, откройте `http://127.0.0.1:5173`; Vite проксирует `/api` на backend.

```sh
make test
HAPPY_TEST_XRAY=$PWD/bin/xray go test -mod=vendor ./internal/gateway -run TestRealXray -v
```

`make test` проверяет разбор подписок, безопасность маршрутов, откат процесса, исчезновение узла, сохранность ключей, отзыв доступа при отказе БД, авторизацию и Origin. Реальная проверка совместимости конфигураций Xray выполняется отдельно и включена в CI. Для полного теста WireGuard см. [integration/README.md](../integration/README.md).

При изменении зависимостей Go выполните `go mod tidy`, затем `make vendor`. Архив `packaging/vendor.tar.gz` содержит vendor-дерево с лицензиями; распакованное дерево исключено из git. Для обновления панели выполните `make ui`. `web/package-lock.json` фиксирует npm-зависимости; overrides фиксируют транзитивные версии для воспроизводимой сборки.

## CI и проверка сборок

Workflow [Check](../.github/workflows/check.yml) запускает Go-тесты с `-race`, `go vet`, сборку TypeScript/Vite, проверки конфигурации настоящим Xray, интеграционные тесты WireGuard и сборку Debian-пакета. Отдельные задания проверяют Nix и собирают Docker-образ для amd64/arm64. Публикация образов выполняется для основной ветки и тегов версий после успешных проверок.

Для локальных сетевых тестов нужны разрешенные TCP/UDP-сокеты; Docker и Nix проверяются при наличии соответствующего окружения. Отсутствие инструмента локально не означает, что его проверки пройдены. Для Nix при необходимости создайте `flake.lock` командой `nix flake lock`.
