# WireGuard end-to-end test

Тест использует настоящий закреплённый Xray и wireguard-go/netstack в userspace. Root, `/dev/net/tun`, внешняя сеть и подписка провайдера во время теста не нужны. Для загрузки зависимостей и Xray сеть нужна только на этапе подготовки.

```sh
sh scripts/fetch-xray.sh amd64 ./bin # Linux arm64: arm64; macOS arm64: darwin-arm64
cd integration
HAPPY_TEST_XRAY=$PWD/../bin/xray go test -mod=mod -race -v -timeout 120s ./...
```

Поднимаются локальный VLESS-провайдер и TCP/UDP цели. Проверяются отсутствие выхода без подписки, TCP и DNS-подобный UDP через WireGuard → VLESS, блокировка LAN/IPv6, отзыв старых ключей и блокировка при исчезновении выбранного узла. Публичные адреса внутри туннеля перенаправляются тестовым провайдером на локальные цели; реальные внешние запросы не выполняются.

Без `HAPPY_TEST_XRAY` тест пропускается. В окружении, запрещающем bind TCP/UDP, этот тест выполнить нельзя. CI запускает его на Linux.
