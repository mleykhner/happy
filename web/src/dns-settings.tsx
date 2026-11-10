import { useState } from "react";
import { Button } from "./button";

export type DNSUpstream = { protocol: string; address: string; ip: string; tlsName: string };
export type DNSSettings = {
  enabled: boolean; listen: string; port: number; failureMode: string;
  primary: DNSUpstream; fallback: DNSUpstream; restrictClients: boolean;
  allowedNetworks: string[]; cacheSize: number; autoWireGuard: boolean; clientDns: string;
};
export type DNSStatus = { running: boolean; route: string; error: string; queries: number; cacheHits: number };
const providers = [
  { name: "Cloudflare", host: "cloudflare-dns.com", ip: "1.1.1.1", tls: "one.one.one.one" },
  { name: "Google", host: "dns.google", ip: "8.8.8.8", tls: "dns.google" },
];
function Upstream({ title, value, onChange, check, checking }: { title: string; value: DNSUpstream; onChange: (v: DNSUpstream) => void; check: () => void; checking: boolean }) {
  return <fieldset className="dns-upstream"><legend>{title}</legend>
    <div className="field-pair">
      <label>Протокол<select value={value.protocol} onChange={e => {
        const protocol = e.target.value;
        onChange({ protocol, address: protocol === "doh" ? "https://cloudflare-dns.com/dns-query" : protocol === "dot" ? "one.one.one.one:853" : "1.1.1.1:53", ip: "", tlsName: "" });
      }}><option value="udp">DNS · UDP → TCP</option><option value="tcp">DNS · TCP</option><option value="doh">DoH · HTTPS</option><option value="dot">DoT · TLS</option></select></label>
      <label>Провайдер<select value="" onChange={e => {
        const p = providers.find(p => p.name === e.target.value); if (!p) return;
        onChange({ ...value, address: value.protocol === "doh" ? `https://${p.host}/dns-query` : value.protocol === "dot" ? `${p.tls}:853` : `${p.ip}:53`, ip: "", tlsName: "" });
      }}><option value="">Своя настройка</option>{providers.map(p => <option key={p.name}>{p.name}</option>)}</select></label>
    </div>
    <label>{value.protocol === "doh" ? "URL DoH" : "Адрес сервера и порт"}<input required value={value.address} onChange={e => onChange({ ...value, address: e.target.value })} spellCheck={false} autoCapitalize="off" /></label>
    {(value.protocol === "doh" || value.protocol === "dot") && <div className="field-pair">
      <label>IP подключения<input value={value.ip} onChange={e => onChange({ ...value, ip: e.target.value })} placeholder="Автоматически" spellCheck={false} /><small>Необязательно. Иначе используется DNS хоста.</small></label>
      <label>TLS-домен<input value={value.tlsName} onChange={e => onChange({ ...value, tlsName: e.target.value })} placeholder="Из адреса сервера" spellCheck={false} /><small>Имя для SNI и проверки сертификата.</small></label>
    </div>}
    <Button type="button" variant="outline" loading={checking} onClick={check}>Проверить DNS</Button>
  </fieldset>;
}
export function DNSSettingsForm({ value, onChange, status, check, managedPort }: { managedPort: number; value: DNSSettings; onChange: (v: DNSSettings) => void; status: DNSStatus; check: (u: DNSUpstream, direct: boolean) => Promise<{ latencyMs: number }> }) {
  const [result, setResult] = useState("");
  const [resultError, setResultError] = useState(false);
  const [checking, setChecking] = useState(false);
  const test = async (u: DNSUpstream, direct: boolean) => {
    if (checking) return;
    setChecking(true); setResultError(false); setResult("Проверяем DNS…");
    try { const r = await check(u, direct); setResult(`DNS отвечает · ${r.latencyMs} мс`); }
    catch (e) { setResultError(true); setResult(e instanceof Error ? e.message : "Не удалось проверить DNS"); }
    finally { setChecking(false); }
  };
  return <section className="dns-settings" aria-labelledby="dns-heading">
    <div className="section-title"><div><h3 id="dns-heading">DNS-прокси</h3><p className="muted">Запросы клиентов → выбранный DNS через Xray</p></div><span className={`status ${status.running && status.route === "primary" ? "good" : ""}`}><i />{!status.running ? "Выключен" : status.route === "primary" ? "Через Xray" : status.route === "fallback" ? "Резервный путь" : "Заблокирован"}</span></div>
    <label className="dns-toggle"><input type="checkbox" checked={value.enabled} onChange={e => onChange({ ...value, enabled: e.target.checked })} /><span>Включить DNS-прокси<small>Принимает обычный DNS по UDP и TCP.</small></span></label>
    {status.error && <p className="alert" role="alert">{status.error}</p>}
    <fieldset className="dns-controls" disabled={!value.enabled} hidden={!value.enabled}>
      <legend className="sr-only">Настройки DNS-прокси</legend>
      <div className="dns-counters"><span>Запросы <strong>{status.queries}</strong></span><span>Из кеша <strong>{status.cacheHits}</strong></span></div>
      <div className="field-pair">
        <label>Адрес прослушивания<input required value={value.listen} onChange={e => onChange({ ...value, listen: e.target.value })} placeholder="0.0.0.0" spellCheck={false} /></label>
        <label>Порт UDP / TCP<input required type="number" min={1} max={65535} value={managedPort || value.port} readOnly={!!managedPort} aria-describedby="dns-port-help" onChange={e => onChange({ ...value, port: Number(e.target.value) })} /></label>
      </div>
      <small id="dns-port-help">{managedPort ? "Порт задан в dnsPort / HAPPY_DNS_PORT. Для изменения обновите конфигурацию сервиса." : "Откройте порт в firewall или Docker. Порт 53 требует разрешения на привилегированные порты."}</small>
      <Upstream title="Основной DNS · через Xray" value={value.primary} onChange={primary => onChange({ ...value, primary })} check={() => test(value.primary, false)} checking={checking} />
      <label>При сбое основного DNS<select value={value.failureMode} onChange={e => onChange({ ...value, failureMode: e.target.value })}><option value="block">Блокировать запросы</option><option value="fallback">Резервный DNS через хост</option></select></label>
      {value.failureMode === "fallback" && <Upstream title="Резервный DNS · через хост" value={value.fallback} onChange={fallback => onChange({ ...value, fallback })} check={() => test(value.fallback, true)} checking={checking} />}
      {result && <p className={resultError ? "alert" : "notice"} role="status">{result}</p>}
      <small>Проверка основного DNS требует сохранённых настроек и активного Xray.</small>
      <details className="advanced-settings"><summary>Кеш и доступ клиентов</summary><div className="details-body">
        <label>Размер кеша<input required type="number" min={0} max={10000} value={value.cacheSize} onChange={e => onChange({ ...value, cacheSize: Number(e.target.value) })} /><small>Число ответов с учётом TTL. 0 — без кеша.</small></label>
        <label className="dns-toggle"><input type="checkbox" checked={value.restrictClients} onChange={e => onChange({ ...value, restrictClients: e.target.checked })} /><span>Ограничить доступ по подсетям<small>{value.restrictClients ? "Локальные обращения и WireGuard-клиенты разрешены всегда." : "Сейчас доступ разрешён всем, кто может подключиться."}</small></span></label>
        {value.restrictClients && <label>Разрешённые подсети<textarea rows={5} value={value.allowedNetworks.join("\n")} onChange={e => onChange({ ...value, allowedNetworks: e.target.value.split("\n") })} spellCheck={false} /><small>По одной CIDR-подсети в строке.</small></label>}
      </div></details>
    </fieldset>
    <div className="wireguard-dns-policy"><h3>DNS клиентов WireGuard</h3>
      <label className="dns-toggle"><input type="checkbox" checked={value.autoWireGuard} onChange={e => onChange({ ...value, autoWireGuard: e.target.checked })} /><span>Назначать DNS Happy<small>При включённом прокси: 10.77.0.1:53 внутри туннеля.</small></span></label>
      {(!value.autoWireGuard || !value.enabled) && <label>Собственные DNS<input value={value.clientDns} onChange={e => onChange({ ...value, clientDns: e.target.value })} placeholder="1.1.1.1, 8.8.8.8" spellCheck={false} /><small>IP через запятую. Можно оставить пустым.</small></label>}
      <small>После изменения скачайте и импортируйте клиентские конфигурации заново.</small>
    </div>
  </section>;
}
