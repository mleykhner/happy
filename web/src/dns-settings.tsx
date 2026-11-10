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
function Upstream({ title, value, onChange, check }: { title: string; value: DNSUpstream; onChange: (v: DNSUpstream) => void; check: () => void }) {
  return <fieldset className="dns-upstream"><legend>{title}</legend>
    <label>Протокол<select value={value.protocol} onChange={e => {
      const protocol = e.target.value;
      onChange({ protocol, address: protocol === "doh" ? "https://cloudflare-dns.com/dns-query" : protocol === "dot" ? "one.one.one.one:853" : "1.1.1.1:53", ip: "", tlsName: "" });
    }}><option value="udp">DNS · UDP с повтором по TCP</option><option value="tcp">DNS · TCP</option><option value="doh">DoH · HTTPS</option><option value="dot">DoT · TLS</option></select></label>
    <label>Готовый сервер<select value="" onChange={e => {
      const p = providers.find(p => p.name === e.target.value); if (!p) return;
      onChange({ ...value, address: value.protocol === "doh" ? `https://${p.host}/dns-query` : value.protocol === "dot" ? `${p.tls}:853` : `${p.ip}:53`, ip: "", tlsName: "" });
    }}><option value="">Выбрать или указать вручную</option>{providers.map(p => <option key={p.name}>{p.name}</option>)}</select></label>
    <label>{value.protocol === "doh" ? "URL DoH" : "Адрес сервера и порт"}<input required value={value.address} onChange={e => onChange({ ...value, address: e.target.value })} /></label>
    {(value.protocol === "doh" || value.protocol === "dot") && <>
      <label>IP подключения · необязательно<input value={value.ip} onChange={e => onChange({ ...value, ip: e.target.value })} placeholder="Автоматически через DNS хоста" /></label>
      <label>Домен для TLS · необязательно<input value={value.tlsName} onChange={e => onChange({ ...value, tlsName: e.target.value })} placeholder="Из адреса сервера" /><small>Используется для SNI и проверки сертификата.</small></label>
    </>}
    <Button type="button" variant="outline" onClick={check}>Проверить сервер</Button>
  </fieldset>;
}
export function DNSSettingsForm({ value, onChange, status, check }: { value: DNSSettings; onChange: (v: DNSSettings) => void; status: DNSStatus; check: (u: DNSUpstream, direct: boolean) => Promise<{ latencyMs: number }> }) {
  const [result, setResult] = useState("");
  const [checking, setChecking] = useState(false);
  const test = async (u: DNSUpstream, direct: boolean) => {
    if (checking) return; setChecking(true); setResult("Проверяем…");
    try { const r = await check(u, direct); setResult(`Сервер отвечает · ${r.latencyMs} мс`); }
    catch (e) { setResult(e instanceof Error ? e.message : "Ошибка проверки"); }
    finally { setChecking(false); }
  };
  return <div className="dns-settings">
    <h3>DNS-прокси</h3>
    <p className="muted">{status.running ? `Работает · ${status.route === "primary" ? "через Xray" : status.route === "fallback" ? "резервный DNS через хост" : "запросы заблокированы"}` : "Выключен"} · Запросов: {status.queries} · Из кеша: {status.cacheHits}</p>
    {status.error && <p className="alert">{status.error}</p>}
    <label className="dns-toggle"><input type="checkbox" checked={value.enabled} onChange={e => onChange({ ...value, enabled: e.target.checked })} />Включить DNS-прокси</label>
    <label>Адрес прослушивания<input required value={value.listen} onChange={e => onChange({ ...value, listen: e.target.value })} placeholder="0.0.0.0" /></label>
    <label>Порт DNS · UDP и TCP<input required type="number" min={1} max={65535} value={value.port} onChange={e => onChange({ ...value, port: Number(e.target.value) })} /><small>Откройте этот порт в firewall или опубликуйте в Docker. Порт 53 требует разрешения на привилегированные порты.</small></label>
    <Upstream title="Основной DNS · через активный Xray" value={value.primary} onChange={primary => onChange({ ...value, primary })} check={() => test(value.primary, false)} />
    <label>При недоступности основного DNS<select value={value.failureMode} onChange={e => onChange({ ...value, failureMode: e.target.value })}><option value="block">Блокировать DNS-запросы</option><option value="fallback">Резервный DNS через интернет хоста</option></select></label>
    {value.failureMode === "fallback" && <Upstream title="Резервный DNS · напрямую через хост" value={value.fallback} onChange={fallback => onChange({ ...value, fallback })} check={() => test(value.fallback, true)} />}
    {result && <p role="status">{result}</p>}
    <small>Для проверки основного сервера сначала сохраните включённый DNS-прокси и подключите Xray.</small>
    <label>Размер кеша · число ответов<input required type="number" min={0} max={10000} value={value.cacheSize} onChange={e => onChange({ ...value, cacheSize: Number(e.target.value) })} /><small>0 — отключить кеш. Ответы хранятся до истечения TTL.</small></label>
    <label className="dns-toggle"><input type="checkbox" checked={value.restrictClients} onChange={e => onChange({ ...value, restrictClients: e.target.checked })} />Ограничить доступ по подсетям</label>
    {!value.restrictClients && <small>Доступ разрешён всем, кто может подключиться к адресу прослушивания.</small>}
    {value.restrictClients && <label>Разрешённые подсети · по одной в строке<textarea rows={5} value={value.allowedNetworks.join("\n")} onChange={e => onChange({ ...value, allowedNetworks: e.target.value.split("\n") })} /><small>Локальные обращения и WireGuard-клиенты Happy разрешены всегда.</small></label>}
    <label className="dns-toggle"><input type="checkbox" checked={value.autoWireGuard} onChange={e => onChange({ ...value, autoWireGuard: e.target.checked })} />Назначать DNS Happy клиентам WireGuard</label>
    {(!value.autoWireGuard || !value.enabled) && <label>DNS в конфигурациях WireGuard<input value={value.clientDns} onChange={e => onChange({ ...value, clientDns: e.target.value })} placeholder="1.1.1.1, 8.8.8.8" /><small>IP-адреса через запятую. Можно оставить пустым.</small></label>}
    <small>При включённых DNS-прокси и автоназначении клиенты используют 10.77.0.1:53 внутри туннеля. После изменения скачайте и импортируйте конфигурации заново.</small>
  </div>;
}
