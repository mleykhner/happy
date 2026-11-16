# Установка Happy

[← README](../README.md) · [Настройка](configuration.md) · [Диагностика](troubleshooting.md)

## Docker

Нужен Linux Docker Engine с Compose. Публикуются TCP панели и UDP WireGuard; при включении DNS добавьте публикацию его UDP/TCP-порта; `privileged`, `NET_ADMIN`, `/dev/net/tun` и host network не нужны.

```sh
mkdir -p secrets
chmod 700 secrets
# Запишите пароль длиной 12–72 байта в secrets/admin-password.
# Не передавайте пароль аргументом shell-команды и не добавляйте его в git.
chmod 600 secrets/admin-password
# Контейнер работает под UID 10001: файл должен быть доступен этому UID.
sudo chown 10001:10001 secrets/admin-password
docker compose pull
docker compose up -d --no-build
```

Compose использует готовый образ `ghcr.io/mleykhner/happy:latest` для `amd64` или `arm64`. Для обновления повторите `docker compose pull` и `docker compose up -d --no-build`; данные сохраняются в томе `happy-data`. Для сборки из локальных исходников используйте `docker compose up -d --build`.

Можно закрепить конкретную версию или digest через `HAPPY_IMAGE`:

```sh
export HAPPY_IMAGE=ghcr.io/mleykhner/happy:0.1.0
docker compose pull
docker compose up -d --no-build
```

Тег версии доступен после публикации соответствующего Git-тега. GitHub Actions публикует образ только после успешных `test` и `nix`: push в основную ветку обновляет `latest`, push тега `v0.1.0` создаёт `v0.1.0`, `0.1.0` и `0.1`. Для опубликованного коммита также доступен тег `sha-<полный SHA>`. Предварительные версии не обновляют тег `major.minor`. Pull request и остальные ветки проверяют сборку без публикации. Авторизация выполняется встроенным `GITHUB_TOKEN` с `packages: write`, отдельный секрет не нужен.

При первой публикации GHCR создаёт пакет с закрытой видимостью. Для скачивания без авторизации откройте пакет `happy` в GitHub → Package settings → Change visibility → Public. До изменения видимости для скачивания нужен вход в GHCR с доступом к пакету. См. [документацию GitHub Container Registry](https://docs.github.com/ru/packages/working-with-a-github-packages-registry/working-with-the-container-registry).

Панель локально: `http://127.0.0.1:8080`. Для прямого доступа по адресу `http://SERVER_IP:8080` измените публикацию порта в `compose.yaml` на `"8080:8080/tcp"` и откройте TCP 8080 в firewall сервера и у хостинга.

Для доступа по HTTPS настройте reverse proxy, затем в `compose.yaml` задайте `HAPPY_ORIGIN=https://vpn.example.org` и `HAPPY_SECURE_COOKIE=true`. Backend не доверяет произвольным `X-Forwarded-*` заголовкам. При проксировании с другого контейнера поместите его в общую Docker-сеть и используйте `http://happy:8080` как upstream.

При ошибках добавления или обновления подписки см. [руководство по диагностике](troubleshooting.md).

Пример Caddy на хосте:

```caddyfile
vpn.example.org {
    reverse_proxy 127.0.0.1:8080
}
```

После входа:

1. В «Настройках» укажите внешний IP/домен сервера без порта.
2. В «Подписках» добавьте URL провайдера и нажмите «Обновить».
3. Выберите единственный выходной узел.
4. В «Подключении» создайте клиента и импортируйте конфиг/QR в WireGuard.
5. Выполните «Проверить соединение» в обзоре.

Откройте UDP 51820 в firewall сервера и у облачного провайдера. При смене порта в панели измените также публикацию Docker-порта и клиентский конфиг.

## Ubuntu 24.04

Сборка `.deb` выполняется из исходников. Нужны Go ≥1.24, Node.js ≥22.12, npm, curl, unzip, shasum и dpkg-deb:

```sh
make ui
sh scripts/build-deb.sh amd64   # либо arm64
sudo apt install ./dist/happy-gateway_0.1.0_amd64.deb
```

Пакет включает Xray **v26.11.9**, проверенный по SHA-256 из upstream release. Сервис запускается непривилегированным пользователем `happy`. Установка пакета не включает сервис до настройки пароля.

Создайте `/etc/happy/admin-password` с паролем, владельцем `root:happy` и правами `0640`, затем `/etc/happy/environment`:

```ini
HAPPY_PASSWORD_FILE=/etc/happy/admin-password
# Для HTTPS reverse proxy:
# HAPPY_ORIGIN=https://vpn.example.org
# HAPPY_SECURE_COOKIE=true
```

```sh
sudo systemctl enable --now happy
sudo journalctl -u happy -f
```

Панель по умолчанию слушает `127.0.0.1:8080`. Для однократной установки/смены пароля без постоянного password-файла:

```sh
sudo -u happy happy set-password --data-dir /var/lib/happy --password-file /etc/happy/admin-password
sudo systemctl restart happy
```

`HAPPY_PASSWORD_FILE` задаёт начальный пароль, только если администратор ещё не настроен. Далее пароль можно сменить в настройках панели, указав текущий пароль; остальные сессии будут отозваны. Для восстановления доступа используйте `happy set-password` и перезапустите сервис.

## Nix и NixOS

```sh
nix build .#happy
nix run .#happy -- version
```

Пакет включает обёртку с закреплённым Xray v26.11.9. Сборка Go использует локальный архив зависимостей; готовая панель включена в исходники. Node.js на целевой машине не нужен. Поддерживаются `x86_64-linux` и `aarch64-linux`.

Подключите flake как input `happy`, затем:

```nix
{
  imports = [ inputs.happy.nixosModules.default ];
  services.happy-gateway = {
    enable = true;
    passwordFile = "/run/secrets/happy-admin-password";
    openFirewall = true;
    wireGuardPort = 51820;
    # Необязательно: nginx + HTTPS (ACME)
    domain = "vpn.example.org";
  };
  # При включённом domain также настройте ACME:
  security.acme.acceptTerms = true;
  security.acme.defaults.email = "admin@example.org";
  networking.firewall.allowedTCPPorts = [ 80 443 ];
}
```

Для прямого доступа к панели по IP вместо `domain` используйте:

```nix
services.happy-gateway = {
  enable = true;
  passwordFile = "/run/secrets/happy-admin-password";
  listen = "0.0.0.0:8080";
  openFirewall = true;
  wireGuardPort = 51820;
};
networking.firewall.allowedTCPPorts = [ 8080 ];
```

Панель будет доступна по `http://SERVER_IP:8080`. При наличии внешнего firewall откройте там TCP 8080 и UDP 51820.

`passwordFile` — строковый путь к runtime-секрету, например sops-nix/agenix; **не Nix path literal** и не `builtins.readFile`. Содержимое не должно попадать в Nix store. systemd передаёт пароль через `LoadCredential`. Состояние — `/var/lib/happy`; используется `DynamicUser`.

Порт в NixOS-модуле открывает firewall; соответствующий порт приложения меняется в панели. Без `domain` HTTPS и внешний Origin настраиваются самостоятельно через `systemd.services.happy.environment`.
