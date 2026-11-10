import React, { useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import { Button } from "./button";
import "./style.css";

type Node = {
  id: string;
  name: string;
  protocol: string;
  host: string;
  port: number;
};
type Traffic = { available: boolean; upload: number; download: number; uploadRate: number; downloadRate: number };
type NodePing = { id: string; ok: boolean; latencyMs: number; checked: string };
function TrafficMonitor() {
  const [traffic, setTraffic] = useState<Traffic | null>(null);
  const [history, setHistory] = useState<Traffic[]>([]);
  useEffect(() => {
    let stopped = false;
    let timer: ReturnType<typeof setTimeout>;
    async function poll() {
      try {
        const sample = await api<Traffic>("/traffic");
        if (!stopped) {
          setTraffic(sample);
          setHistory(h => [...h.slice(-59), sample]);
        }
      } catch { if (!stopped) setTraffic(null); }
      if (!stopped) timer = setTimeout(poll, 1000);
    }
    poll();
    return () => { stopped = true; clearTimeout(timer); };
  }, []);
  const peak = Math.max(1024, ...history.flatMap(t => [t.downloadRate, t.uploadRate]));
  const points = (key: "downloadRate" | "uploadRate") => history.map((t, i) => `${i * 600 / 59},${100 - (t[key] || 0) / peak * 95}`).join(" ");
  return <section className="card traffic-monitor">
    <div className="section-title"><h2>Трафик в реальном времени</h2><span className="muted">WireGuard · обновление 1 с</span></div>
    {traffic?.available ? <>
      <div className="traffic-values">
        <div><span className="muted">↓ Загрузка</span><h3>{bytes(traffic.downloadRate)}/с</h3><span>Всего {bytes(traffic.download)}</span></div>
        <div><span className="muted">↑ Отправка</span><h3>{bytes(traffic.uploadRate)}/с</h3><span>Всего {bytes(traffic.upload)}</span></div>
      </div>
      <svg viewBox="0 0 600 105" role="img" aria-label="История скорости: загрузка зелёным, отправка синим">
        <polyline className="traffic-download" points={points("downloadRate")} />
        <polyline className="traffic-upload" points={points("uploadRate")} />
      </svg>
      <p className="muted">Счётчики с запуска Xray; сбрасываются при применении конфигурации. Объём полезного трафика клиента, включая заблокированные запросы.</p>
    </> : <p className="muted">Статистика недоступна. Ожидаем Xray…</p>}
  </section>;
}
type SubscriptionMetadata = {
  title?: string;
  description?: string;
  announcement?: string;
  website?: string;
  support?: string;
  announcementUrl?: string;
  upload?: number;
  download?: number;
  total?: number;
  expire?: number;
};
type Subscription = {
  metadata?: SubscriptionMetadata;
  id: string;
  name: string;
  nodes: Node[];
  updated: string;
  error: string;
};
type Settings = {
  endpoint: string;
  wireguardPort: number;
  refreshMinutes: number;
};
type Health = {
  checked: string;
  ok: boolean;
  latencyMs: number;
  error: string;
};
type State = {
  deviceId: string;
  settings: Settings;
  subscriptions: Subscription[];
  selected: string;
  client: null | { enabled: boolean; publicKey: string };
  running: boolean;
  exitDisabled: boolean;
  blocked: boolean;
  health: Health;
  events: { time: string; message: string }[];
};
class Unauthorized extends Error {}
async function api<T = unknown>(
  path: string,
  method = "GET",
  body?: unknown,
): Promise<T> {
  const r = await fetch(`/api${path}`, {
    method,
    credentials: "same-origin",
    headers: body === undefined ? {} : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = await r.json();
  if (r.status === 401) throw new Unauthorized(data.error);
  if (!r.ok) throw new Error(data.error || "Не удалось выполнить запрос");
  return data;
}
function date(raw: string) {
  return !raw || raw.startsWith("0001")
    ? "Ещё не обновлялась"
    : new Date(raw).toLocaleString("ru-RU");
}
function bytes(value: number) {
  const units = ["Б", "КиБ", "МиБ", "ГиБ", "ТиБ", "ПиБ"];
  const unit =
    value > 0
      ? Math.max(0, Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1))
      : 0;
  return `${new Intl.NumberFormat("ru-RU", { maximumFractionDigits: 2 }).format(value / 1024 ** unit)} ${units[unit]}`;
}
function ProviderInfo({ subscription }: { subscription: Subscription }) {
  const m = subscription.metadata;
  if (!m || Object.keys(m).length === 0)
    return <p className="muted">Провайдер не передал сведения о подписке.</p>;
  const used =
    m.upload !== undefined && m.download !== undefined
      ? m.upload + m.download
      : undefined;
  const expired =
    m.expire !== undefined && m.expire > 0 && m.expire * 1000 < Date.now();
  const exhausted =
    m.total !== undefined &&
    m.total > 0 &&
    used !== undefined &&
    used >= m.total;
  return (
    <div className="provider-info">
      {m.title && <h3>{m.title}</h3>}
      {m.description && <p className="provider-text">{m.description}</p>}
      <div className="provider-stats">
        {(m.upload !== undefined ||
          m.download !== undefined ||
          m.total !== undefined) && (
          <div>
            <p className="eyebrow">ТРАФИК ПРОВАЙДЕРА</p>
            <strong>
              {used !== undefined ? bytes(used) : "Расход не указан"}
              {m.total !== undefined
                ? m.total === 0
                  ? " · без лимита"
                  : ` / ${bytes(m.total)}`
                : " · лимит не указан"}
            </strong>
            {used !== undefined && m.total !== undefined && m.total > 0 && (
              <>
                <progress
                  aria-label="Использованный трафик"
                  value={Math.min(used, m.total)}
                  max={m.total}
                />
                <small>
                  {exhausted
                    ? "Лимит исчерпан"
                    : `Осталось ${bytes(Math.max(0, m.total - used))}`}
                </small>
              </>
            )}
            <p className="muted">
              ↑ {m.upload !== undefined ? bytes(m.upload) : "Нет данных"} · ↓{" "}
              {m.download !== undefined ? bytes(m.download) : "Нет данных"}
            </p>
          </div>
        )}
        {m.expire !== undefined && (
          <div>
            <p className="eyebrow">ДЕЙСТВУЕТ ДО</p>
            <strong>
              {m.expire === 0
                ? "Без срока окончания"
                : date(new Date(m.expire * 1000).toISOString())}
            </strong>
            {expired && <p className="alert">Срок подписки истёк</p>}
          </div>
        )}
      </div>
      {m.announcement && (
        <div className="notice provider-text">{m.announcement}</div>
      )}
      <div className="actions provider-links">
        {(
          [
            [m.website, "Сайт провайдера ↗"],
            [m.support, "Поддержка ↗"],
            [m.announcementUrl, "Подробнее об объявлении ↗"],
          ] as const
        ).map(
          ([href, label]) =>
            href && (
              <a
                key={label}
                href={href}
                target="_blank"
                rel="noopener noreferrer"
              >
                {label}
              </a>
            ),
        )}
      </div>
      <small className="muted">
        Данные провайдера на {date(subscription.updated)}.
      </small>
    </div>
  );
}
function App() {
  const [state, setState] = useState<State | null>(null);
  const [authed, setAuthed] = useState<boolean | null>(null);
  const [tab, setTab] = useState("overview");
  const [password, setPassword] = useState("");
  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [name, setName] = useState("");
  const [url, setURL] = useState("");
  const [query, setQuery] = useState("");
  const [pings, setPings] = useState<Record<string, NodePing>>({});
  const [pinging, setPinging] = useState(false);
  const [settings, setSettings] = useState<Settings | null>(null);
  const [qr, setQR] = useState(false);
  const load = async () => {
    const s = await api<State>("/state");
    setState(s);
    setAuthed(true);
    return s;
  };
  const report = (e: unknown) => {
    if (e instanceof Unauthorized) {
      setAuthed(false);
      setState(null);
      setQR(false);
    }
    setError(e instanceof Error ? e.message : "Ошибка соединения");
  };
  useEffect(() => {
    load().catch((e) => {
      if (e instanceof Unauthorized) setAuthed(false);
      else report(e);
    });
  }, []);
  useEffect(() => {
    if (!authed) return;
    const id = setInterval(() => {
      load().catch(report);
    }, 15000);
    return () => clearInterval(id);
  }, [authed]);
  async function action(
    fn: () => Promise<unknown>,
    message = "Изменения сохранены",
  ) {
    setBusy(true);
    setError("");
    setNotice("");
    try {
      await fn();
      await load();
      setNotice(message);
    } catch (e) {
      report(e);
      await load().catch(() => {});
    } finally {
      setBusy(false);
    }
  }
  const nodes =
    state?.subscriptions.flatMap((sub) =>
      sub.nodes.map((n) => ({ ...n, subscription: sub.name })),
    ) ?? [];
  const selected = nodes.find((n) => n.id === state?.selected);
  const selectedSubscription = state?.subscriptions.find((sub) =>
    sub.nodes.some((n) => n.id === state.selected),
  );
  const tabs = [
    ["overview", "Обзор"],
    ["subscriptions", "Подписки"],
    ["client", "Подключение"],
    ["settings", "Настройки"],
  ];
  if (authed === null)
    return (
      <main className="login">
        <div className="wordmark">
          h<span>appy</span>
          <i />
        </div>
        <p>Подключаемся к шлюзу…</p>
        {error && (
          <div role="alert" className="alert">
            {error}
          </div>
        )}
      </main>
    );
  if (!authed)
    return (
      <main className="login">
        <div className="wordmark">
          h<span>appy</span>
          <i />
        </div>
        <p className="eyebrow">ВАШ ЛИЧНЫЙ VPN-ШЛЮЗ</p>
        <h1>С возвращением.</h1>
        <p className="muted">Войдите, чтобы управлять подключением.</p>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            setBusy(true);
            setError("");
            api("/login", "POST", { password })
              .then(() => {
                setPassword("");
                return load();
              })
              .catch(report)
              .finally(() => setBusy(false));
          }}
        >
          <label>
            Пароль администратора
            <input
              autoFocus
              type="password"
              autoComplete="current-password"
              required
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          </label>
          {error && (
            <div className="alert" role="alert">
              {error}
            </div>
          )}
          <Button disabled={busy} type="submit">
            {busy ? "Входим…" : "Войти в панель →"}
          </Button>
        </form>
        <small>Один сервер. Одно подключение. Ваш контроль.</small>
      </main>
    );
  if (!state) return null;
  return (
    <div className="shell">
      <aside>
        <a className="wordmark" href="/">
          h<span>appy</span>
          <i />
        </a>
        <p className="eyebrow">ЛИЧНЫЙ ШЛЮЗ</p>
        <nav>
          {tabs.map(([id, label], i) => (
            <button
              key={id}
              onClick={() => {
                setTab(id);
                setQR(false);
                setNotice("");
                setError("");
                setCurrentPassword("");
                setNewPassword("");
                setConfirmPassword("");
                if (id === "settings") setSettings({ ...state.settings });
              }}
              className={tab === id ? "active" : ""}
            >
              <span className="nav-number">0{i + 1}</span>
              {label}
            </button>
          ))}
        </nav>
        <div className="aside-bottom">
          <span className="pill">XRAY CORE</span>
          <p>
            Приватный маршрут
            <br />
            под вашим управлением.
          </p>
          <Button
            variant="outline"
            disabled={busy}
            onClick={() => {
              setBusy(true);
              api("/logout", "POST")
                .then(() => {
                  setAuthed(false);
                  setState(null);
                  setQR(false);
                })
                .catch(report)
                .finally(() => setBusy(false));
            }}
          >
            Выйти ↗
          </Button>
        </div>
      </aside>
      <main className="content">
        <header>
          <div>
            <p className="eyebrow">ПАНЕЛЬ УПРАВЛЕНИЯ</p>
            <h1>{tabs.find((t) => t[0] === tab)?.[1]}</h1>
          </div>
          <span className={`status ${state.running ? "good" : ""}`}>
            <i />
            {state.running ? "Xray запущен" : "Xray остановлен"}
          </span>
        </header>
        {error && (
          <div role="alert" className="alert">
            {error}
          </div>
        )}
        {notice && (
          <div role="status" className="notice">
            {notice}
          </div>
        )}
        {tab === "overview" && (
          <>
            <TrafficMonitor />
            <section className="card connection-card">
              <p className="eyebrow">WIREGUARD → XRAY</p>
              <button
                className={`connection-toggle ${!state.exitDisabled && selected && state.running ? "connected" : ""}`}
                disabled={busy || !selected}
                aria-pressed={
                  !state.exitDisabled && !!selected && state.running
                }
                onClick={() =>
                  action(
                    () =>
                      api("/connection", "POST", {
                        enabled: state.exitDisabled || !state.running,
                      }),
                    state.exitDisabled || !state.running
                      ? "Выход включён"
                      : "Выход отключён",
                  )
                }
              >
                <span className="power-icon" aria-hidden="true">
                  ⏻
                </span>
                <strong>
                  {busy
                    ? "Применяем…"
                    : state.exitDisabled
                      ? "Подключить"
                      : selected && state.running
                        ? "Отключить"
                        : "Подключить"}
                </strong>
                <span>
                  {state.exitDisabled
                    ? "Отключено"
                    : !selected
                      ? "Выберите сервер"
                      : !state.running
                        ? "Xray остановлен"
                        : "Выход включён"}
                </span>
              </button>
              <h2>{selected?.name || "Сервер не выбран"}</h2>
              <p className="muted">
                {selected
                  ? `${selected.subscription} · ${selected.protocol.toUpperCase()} · ${selected.host}:${selected.port}`
                  : "Добавьте подписку и выберите сервер в списке ниже."}
              </p>
              <p className="muted">
                {state.blocked
                  ? "Клиентский трафик заблокирован."
                  : "Маршрут для WireGuard-клиента настроен."}{" "}
                {!state.client
                  ? "Создайте WireGuard-клиент в разделе «Подключение»."
                  : !state.client.enabled
                    ? "WireGuard-клиент отключён."
                    : ""}
              </p>
              <Button
                variant="outline"
                disabled={
                  busy || state.exitDisabled || !selected || !state.running
                }
                onClick={() =>
                  action(() => api("/probe", "POST"), "Проверка завершена")
                }
              >
                Проверить доступность
              </Button>
            </section>
            {selectedSubscription && (
              <section className="card">
                <h2>Подписка · {selectedSubscription.name}</h2>
                <ProviderInfo subscription={selectedSubscription} />
                {selectedSubscription.error && (
                  <p className="alert">
                    Обновление не удалось; показаны последние сохранённые
                    данные.
                  </p>
                )}
              </section>
            )}
            <section className="card">
              <div className="section-title">
                <h2>
                  Все серверы <span className="count">{nodes.length}</span>
                </h2>
                <Button variant="outline" disabled={pinging || nodes.length === 0} onClick={async () => {
                  setPinging(true);
                  try { const results = await api<NodePing[]>("/ping", "POST"); setPings(Object.fromEntries(results.filter(p => p.id).map(p => [p.id, p]))); }
                  catch (e) { report(e); }
                  finally { setPinging(false); }
                }}>{pinging ? "Проверяем…" : "Проверить пинг всех"}</Button>
              </div>
              <input
                aria-label="Поиск узлов"
                placeholder="Поиск по названию или провайдеру…"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
              />
              <div className="nodes">
                {nodes
                  .filter((n) =>
                    `${n.name} ${n.subscription}`
                      .toLowerCase()
                      .includes(query.toLowerCase()),
                  )
                  .map((n) => (
                    <div
                      className={`node ${n.id === state.selected ? "selected" : ""}`}
                      key={`${n.subscription}-${n.id}`}
                    >
                      <div>
                        <h3>{n.name}</h3>
                        <p>
                          {n.subscription} · {n.protocol.toUpperCase()} ·{" "}
                          {n.host}:{n.port}
                        </p>
                        <p className="muted" title={pings[n.id] ? date(pings[n.id].checked) : "Проверка доступности TCP-порта со шлюза"}>
                          TCP-пинг: {pinging ? "проверяем…" : !pings[n.id] ? "не проверен" : pings[n.id].ok ? `${pings[n.id].latencyMs} мс` : "недоступен"}
                        </p>
                      </div>
                      <Button
                        variant={
                          n.id === state.selected ? "default" : "outline"
                        }
                        disabled={busy || n.id === state.selected}
                        onClick={() =>
                          action(
                            () => api("/select", "POST", { id: n.id }),
                            "Единый выход изменён",
                          )
                        }
                      >
                        {n.id === state.selected ? "Выбран ✓" : "Выбрать"}
                      </Button>
                    </div>
                  ))}
                {nodes.length === 0 && (
                  <p className="empty">
                    Здесь появятся узлы после обновления подписки.
                  </p>
                )}
              </div>
            </section>
            <div className="stats">
              <article className="card">
                <p className="eyebrow">ЕДИНЫЙ ВЫХОД</p>
                <h3>{selected?.name || "Не выбран"}</h3>
                <p className="muted">
                  {selected
                    ? `${selected.protocol.toUpperCase()} · ${selected.host}`
                    : "Добавьте подписку и выберите узел"}
                </p>
              </article>
              <article className="card">
                <p className="eyebrow">ПОДКЛЮЧЕНИЕ</p>
                <h3>
                  {state.client
                    ? state.client.enabled
                      ? "Включено"
                      : "Отключено"
                    : "Не создано"}
                </h3>
                <p className="muted">WireGuard · один клиент</p>
              </article>
              <article className="card">
                <p className="eyebrow">ПРОВЕРКА ВЫХОДА</p>
                <h3>
                  {state.health.checked.startsWith("0001")
                    ? "Не проверен"
                    : state.health.ok
                      ? `${state.health.latencyMs} мс`
                      : "Недоступен"}
                </h3>
                <p className="muted">
                  {state.health.error ||
                    (state.health.checked.startsWith("0001")
                      ? "Запустите проверку соединения"
                      : date(state.health.checked))}
                </p>
              </article>
            </div>
            <section className="card">
              <div className="section-title">
                <h2>События</h2>
                <span className="muted">Текущий запуск</span>
              </div>
              {state.events.length === 0 ? (
                <p className="muted">Событий пока нет</p>
              ) : (
                [...state.events]
                  .reverse()
                  .slice(0, 12)
                  .map((e, i) => (
                    <div className="event" key={i}>
                      <span>{e.message}</span>
                      <time>{date(e.time)}</time>
                    </div>
                  ))
              )}
            </section>
          </>
        )}
        {tab === "subscriptions" && (
          <>
            <section className="card">
              <h2>Добавить подписку</h2>
              <p className="muted">
                Ссылка от провайдера. Адрес хранится на сервере и скрыт в
                панели.
              </p>
              <form
                className="subscription-form"
                onSubmit={(e) => {
                  e.preventDefault();
                  action(async () => {
                    await api("/subscriptions", "POST", { name, url });
                    setName("");
                    setURL("");
                  }, "Подписка добавлена. Нажмите «Обновить», чтобы загрузить узлы.");
                }}
              >
                <label>
                  Название
                  <input
                    required
                    maxLength={120}
                    placeholder="Мой провайдер"
                    value={name}
                    onChange={(e) => setName(e.target.value)}
                  />
                </label>
                <label>
                  URL подписки
                  <input
                    type="url"
                    required
                    autoComplete="off"
                    placeholder="https://provider.example/sub/…"
                    value={url}
                    onChange={(e) => setURL(e.target.value)}
                  />
                </label>
                <Button type="submit" disabled={busy}>
                  Добавить +
                </Button>
              </form>
            </section>
            {state.subscriptions.map((sub) => (
              <section className="card" key={sub.id}>
                <div className="section-title">
                  <div>
                    <h2>{sub.name}</h2>
                    <p className="muted">
                      {sub.nodes.length} узлов · {date(sub.updated)}
                    </p>
                  </div>
                  <div className="actions">
                    <Button
                      variant="outline"
                      disabled={busy}
                      onClick={() =>
                        action(
                          () => api(`/subscriptions/${sub.id}/refresh`, "POST"),
                          "Подписка обновлена",
                        )
                      }
                    >
                      Обновить
                    </Button>
                    <Button
                      variant="danger"
                      disabled={busy}
                      onClick={() => {
                        if (
                          confirm(
                            "Удалить подписку? Если её узел выбран, выход будет заблокирован.",
                          )
                        )
                          action(() =>
                            api(`/subscriptions/${sub.id}`, "DELETE"),
                          );
                      }}
                    >
                      Удалить
                    </Button>
                  </div>
                </div>
                <ProviderInfo subscription={sub} />
                {sub.error && (
                  <p className="alert">
                    {sub.error} Показаны последние сохранённые данные.
                  </p>
                )}
              </section>
            ))}
          </>
        )}
        {tab === "client" && (
          <section className="card client-card">
            <span className="pill">ОДНО УСТРОЙСТВО</span>
            <h2>Ваш WireGuard-клиент</h2>
            <p className="muted">
              Установите WireGuard на устройстве и импортируйте конфиг или
              отсканируйте QR-код.
            </p>
            {!state.client ? (
              <Button
                disabled={busy}
                onClick={() =>
                  action(
                    () => api("/client/create", "POST"),
                    "Подключение создано",
                  )
                }
              >
                Создать подключение +
              </Button>
            ) : (
              <>
                <dl>
                  <dt>Состояние</dt>
                  <dd>{state.client.enabled ? "Включено" : "Отключено"}</dd>
                  <dt>Адрес клиента</dt>
                  <dd>10.77.0.2</dd>
                  <dt>Публичный ключ</dt>
                  <dd className="key">{state.client.publicKey}</dd>
                </dl>
                {!state.settings.endpoint && (
                  <div className="alert">
                    Укажите внешний адрес сервера в настройках перед скачиванием
                    конфигурации.
                  </div>
                )}
                <div className="actions">
                  <Button asChild variant="outline">
                    <a href="/api/client/config" download="happy.conf">
                      Скачать конфиг ↓
                    </a>
                  </Button>
                  <Button
                    variant="outline"
                    disabled={!state.settings.endpoint}
                    onClick={() => setQR(!qr)}
                  >
                    {qr ? "Скрыть QR" : "Показать QR"}
                  </Button>
                  <Button
                    disabled={busy}
                    variant="outline"
                    onClick={() =>
                      action(() =>
                        api(
                          `/client/${state.client?.enabled ? "disable" : "enable"}`,
                          "POST",
                        ),
                      )
                    }
                  >
                    {state.client.enabled ? "Отключить" : "Включить"}
                  </Button>
                  <Button
                    variant="danger"
                    disabled={busy}
                    onClick={() => {
                      if (
                        confirm(
                          "Перевыпустить ключи? Старый конфиг перестанет работать после успешного применения.",
                        )
                      ) {
                        setQR(false);
                        action(
                          () => api("/client/rotate", "POST"),
                          "Ключи перевыпущены. Импортируйте новый конфиг.",
                        );
                      }
                    }}
                  >
                    Перевыпустить ключи
                  </Button>
                </div>
                {qr && (
                  <div className="qr">
                    <img
                      src="/api/client/qr"
                      width="320"
                      height="320"
                      alt="QR-код конфигурации WireGuard"
                    />
                    <p className="muted">
                      QR содержит приватный ключ. Не передавайте его
                      посторонним.
                    </p>
                  </div>
                )}
                <p className="footnote">
                  Весь трафик → выбранный узел. DNS → через прокси. IPv6 →
                  блокируется на шлюзе.
                </p>
              </>
            )}
          </section>
        )}
        {tab === "settings" && settings && (
          <>
            <section className="card">
              <h2>Настройки шлюза</h2>
              <label>
                HWID шлюза
                <input readOnly value={state.deviceId} />
                <small>
                  Постоянный ID для привязки подписки у провайдера. Сохраняется
                  вместе с данными приложения.
                </small>
              </label>
              <form
                className="settings-form"
                onSubmit={(e) => {
                  e.preventDefault();
                  action(() => api("/settings", "PUT", settings));
                }}
              >
                <label>
                  Внешний IP или домен сервера
                  <input
                    required
                    value={settings.endpoint}
                    onChange={(e) =>
                      setSettings({ ...settings, endpoint: e.target.value })
                    }
                    placeholder="vpn.example.org"
                  />
                  <small>
                    Без схемы и порта. Этот адрес попадёт в конфиг клиента.
                  </small>
                </label>
                <label>
                  UDP-порт WireGuard
                  <input
                    type="number"
                    min={1024}
                    max={65535}
                    required
                    value={settings.wireguardPort}
                    onChange={(e) =>
                      setSettings({
                        ...settings,
                        wireguardPort: Number(e.target.value),
                      })
                    }
                  />
                  <small>
                    При изменении обновите публикацию порта Docker или firewall
                    и конфиг клиента.
                  </small>
                </label>
                <label>
                  Обновление подписок, минут
                  <input
                    type="number"
                    min={5}
                    max={10080}
                    required
                    value={settings.refreshMinutes}
                    onChange={(e) =>
                      setSettings({
                        ...settings,
                        refreshMinutes: Number(e.target.value),
                      })
                    }
                  />
                </label>
                <Button type="submit" disabled={busy}>
                  Сохранить настройки
                </Button>
              </form>
            </section>
            <section className="card">
              <h2>Пароль администратора</h2>
              <p className="muted">
                После смены пароля остальные сессии будут завершены.
              </p>
              <form
                className="settings-form"
                onSubmit={(e) => {
                  e.preventDefault();
                  if (newPassword !== confirmPassword) {
                    setError("Новые пароли не совпадают");
                    return;
                  }
                  action(async () => {
                    await api("/password", "PUT", {
                      currentPassword,
                      newPassword,
                    });
                    setCurrentPassword("");
                    setNewPassword("");
                    setConfirmPassword("");
                  }, "Пароль изменён. Остальные сессии завершены.");
                }}
              >
                <label>
                  Текущий пароль
                  <input
                    type="password"
                    autoComplete="current-password"
                    required
                    value={currentPassword}
                    onChange={(e) => setCurrentPassword(e.target.value)}
                  />
                </label>
                <label>
                  Новый пароль
                  <input
                    type="password"
                    autoComplete="new-password"
                    required
                    minLength={12}
                    value={newPassword}
                    onChange={(e) => setNewPassword(e.target.value)}
                  />
                  <small>
                    От 12 до 72 байт; для кириллицы один символ занимает
                    несколько байт.
                  </small>
                </label>
                <label>
                  Повторите новый пароль
                  <input
                    type="password"
                    autoComplete="new-password"
                    required
                    minLength={12}
                    value={confirmPassword}
                    onChange={(e) => setConfirmPassword(e.target.value)}
                  />
                </label>
                <Button type="submit" disabled={busy}>
                  Сменить пароль
                </Button>
              </form>
            </section>
          </>
        )}
        <footer>
          <span>happy / 0.1</span>
          <span>Без прямого выхода. Под вашим контролем.</span>
        </footer>
      </main>
    </div>
  );
}
createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
