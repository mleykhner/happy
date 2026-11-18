package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
)

// Only network configuration is retained. Provider listeners, file paths, env,
// APIs and logging never become part of the gateway's runtime configuration.
type SubscriptionXrayConfig struct {
	Outbounds        []map[string]any `json:"outbounds"`
	Routing          map[string]any   `json:"routing,omitempty"`
	DNS              map[string]any   `json:"dns,omitempty"`
	Observatory      map[string]any   `json:"observatory,omitempty"`
	BurstObservatory map[string]any   `json:"burstObservatory,omitempty"`
	Policy           map[string]any   `json:"policy,omitempty"`
	ClientTags       []string         `json:"clientTags,omitempty"`
}

func wireguardDocument(body string) bool {
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		return strings.EqualFold(line, "[Interface]")
	}
	return false
}

func subscriptionRoutingDirective(line string) bool {
	for _, prefix := range []string{"incy", "happ", ""} {
		for _, action := range []string{"autorouting/", "routing/", "onadd/"} {
			if strings.HasPrefix(line, prefix+"://"+action) {
				return true
			}
		}
	}
	return false
}

func parseJSONSubscription(body string) ([]Node, error) {
	var entries []json.RawMessage
	if strings.HasPrefix(body, "[") {
		if json.Unmarshal([]byte(body), &entries) != nil {
			return nil, errors.New("некорректный JSON подписки")
		}
	} else {
		entries = []json.RawMessage{json.RawMessage(body)}
	}
	if len(entries) > 2000 {
		return nil, errors.New("слишком много конфигураций")
	}
	nodes := []Node{}
	seen := map[string]bool{}
	skipped := 0
	for i, entry := range entries {
		n, err := parseXraySubscriptionConfig(entry)
		if errors.Is(err, errUnsupportedNodeProtocol) {
			skipped++
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("конфигурация %d: %w", i+1, err)
		}
		if err = providerNotice(n); err != nil {
			return nil, err
		}
		if !seen[n.ID] {
			nodes = append(nodes, n)
			seen[n.ID] = true
		}
	}
	if skipped > 0 {
		log.Printf("subscription.parse unsupported_skipped count=%d", skipped)
	}
	if len(nodes) == 0 {
		return nil, errors.New("в подписке нет поддерживаемых узлов")
	}
	return nodes, nil
}

func parseXraySubscriptionConfig(raw json.RawMessage) (Node, error) {
	var source struct {
		Type     string `json:"type"`
		Remarks  string `json:"remarks"`
		Inbounds []struct {
			Tag      string
			Protocol string
		} `json:"inbounds"`
		SubscriptionXrayConfig
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	if d.Decode(&source) != nil || !json.Valid(raw) {
		return Node{}, errors.New("некорректный JSON конфигурации")
	}
	if source.Type == "amneziawg" {
		return Node{}, errUnsupportedNodeProtocol
	}
	c := source.SubscriptionXrayConfig
	// This internal field is derived from the source listeners, never accepted from JSON.
	c.ClientTags = nil
	for _, inbound := range source.Inbounds {
		if inbound.Tag != "" && (inbound.Protocol == "socks" || inbound.Protocol == "http" || inbound.Protocol == "mixed" || inbound.Protocol == "tun") {
			c.ClientTags = append(c.ClientTags, inbound.Tag)
		}
	}
	if len(c.Outbounds) == 0 || len(c.Outbounds) > 2000 {
		return Node{}, errors.New("нет выходов Xray или превышен лимит выходов")
	}
	var first map[string]any
	tags := map[string]bool{}
	for i, outbound := range c.Outbounds {
		protocol, _ := outbound["protocol"].(string)
		switch protocol {
		case "vless", "vmess", "trojan", "shadowsocks":
			if first == nil {
				first = outbound
			}
			if _, _, err := xrayNodeEndpoint(outbound); err != nil {
				return Node{}, err
			}
		case "freedom", "blackhole", "dns":
		default:
			return Node{}, errUnsupportedNodeProtocol
		}
		tag, _ := outbound["tag"].(string)
		if tag == "" {
			tag = fmt.Sprintf("outbound-%d", i)
			outbound["tag"] = tag
		}
		if tags[tag] {
			return Node{}, errors.New("повторяющийся тег выхода Xray")
		}
		tags[tag] = true
		// Keep only documented outbound fields, not arbitrary per-outbound extensions.
		for key := range outbound {
			switch key {
			case "tag", "protocol", "settings", "streamSettings", "mux", "sendThrough", "targetStrategy":
			case "proxySettings":
				return Node{}, errors.New("устаревший proxySettings не поддерживается; используйте sockopt.dialerProxy")
			default:
				delete(outbound, key)
			}
		}
		if err := validateSubscriptionJSON(outbound); err != nil {
			return Node{}, err
		}
	}
	if first == nil {
		return Node{}, errUnsupportedNodeProtocol
	}
	host, port, _ := xrayNodeEndpoint(first)
	name := source.Remarks
	if name == "" {
		name, _ = first["tag"].(string)
	}
	if name == "" {
		name = host
	}
	canonical, _ := json.Marshal(c)
	return Node{ID: identity(string(canonical)), Name: metadataText(name, 200), Protocol: "xray", Host: host, Port: port, Outbound: first, FullConfig: &c}, nil
}

func xrayNodeEndpoint(outbound map[string]any) (string, int, error) {
	settings, _ := outbound["settings"].(map[string]any)
	endpoint := settings
	for _, key := range []string{"vnext", "servers"} {
		if servers, ok := settings[key].([]any); ok && len(servers) > 0 {
			endpoint, _ = servers[0].(map[string]any)
			break
		}
	}
	host, _ := endpoint["address"].(string)
	port, err := strconv.Atoi(fmt.Sprint(endpoint["port"]))
	if err != nil || port < 1 || port > 65535 || host == "" || strings.ContainsAny(host, " /\r\n\t") {
		return "", 0, errors.New("некорректный адрес или порт узла Xray")
	}
	return host, port, nil
}

func validateSubscriptionJSON(value any) error {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if key == "allowInsecure" && item != false && item != nil {
				return errors.New("небезопасный TLS запрещён")
			}
			switch key {
			case "certificateFile", "keyFile", "masterKeyLog", "keyLogFile":
				if item != "" && item != nil {
					return errors.New("внешние файлы в подписке не поддерживаются")
				}
			}
			if err := validateSubscriptionJSON(item); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range v {
			if err := validateSubscriptionJSON(item); err != nil {
				return err
			}
		}
	}
	return nil
}

func mergeSubscriptionXray(target map[string]any, original *SubscriptionXrayConfig) error {
	// Isolate tag rewriting from persisted state and concurrent View/refresh calls.
	b, err := json.Marshal(original)
	if err != nil {
		return errors.New("не удалось собрать JSON конфигурации")
	}
	var c SubscriptionXrayConfig
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	if d.Decode(&c) != nil || len(c.Outbounds) == 0 {
		return errors.New("некорректная конфигурация Xray")
	}
	prefix := "provider-"
	defaultTag, _ := c.Outbounds[0]["tag"].(string)
	outbounds := target["outbounds"].([]any)
	for _, outbound := range c.Outbounds {
		outbound["tag"] = prefix + outbound["tag"].(string)
		rewriteStreamDialers(outbound["streamSettings"], prefix)
		outbounds = append(outbounds, outbound)
	}
	target["outbounds"] = outbounds
	routing := target["routing"].(map[string]any)
	protected := routing["rules"].([]any)
	catchAll := protected[len(protected)-1].(map[string]any)
	catchAll["outboundTag"] = prefix + defaultTag
	rules := protected[:len(protected)-1]
	if providerRules, ok := c.Routing["rules"].([]any); ok {
		for _, raw := range providerRules {
			rule, ok := raw.(map[string]any)
			if !ok {
				return errors.New("некорректное правило маршрутизации Xray")
			}
			for _, key := range []string{"outboundTag", "balancerTag"} {
				if tag, ok := rule[key].(string); ok {
					rule[key] = prefix + tag
				}
			}
			if tags, ok := rule["inboundTag"].([]any); ok {
				mapped := []any{}
				clientMapped := false
				for _, tag := range tags {
					client := false
					for _, sourceTag := range c.ClientTags {
						if tag == sourceTag {
							client = true
							break
						}
					}
					if client {
						if !clientMapped {
							mapped = append(mapped, "wireguard", "health", "dns-upstream")
							clientMapped = true
						}
					} else if s, ok := tag.(string); ok {
						mapped = append(mapped, prefix+s)
					}
				}
				rule["inboundTag"] = mapped
			}
			rules = append(rules, rule)
		}
	}
	routing["rules"] = append(rules, catchAll)
	for key, item := range c.Routing {
		if key != "rules" && key != "balancers" {
			routing[key] = item
		}
	}
	if balancers, ok := c.Routing["balancers"].([]any); ok {
		for _, raw := range balancers {
			balancer, ok := raw.(map[string]any)
			if !ok {
				return errors.New("некорректный балансировщик Xray")
			}
			if tag, ok := balancer["tag"].(string); ok {
				balancer["tag"] = prefix + tag
			}
			if tag, ok := balancer["fallbackTag"].(string); ok {
				balancer["fallbackTag"] = prefix + tag
			}
			prefixSelectors(balancer, "selector", prefix)
		}
		routing["balancers"] = balancers
	}
	if c.DNS != nil {
		if tag, ok := c.DNS["tag"].(string); ok {
			c.DNS["tag"] = prefix + tag
		}
		target["dns"] = c.DNS
	}
	if c.Policy != nil {
		target["policy"] = c.Policy
	}
	for key, observatory := range map[string]map[string]any{"observatory": c.Observatory, "burstObservatory": c.BurstObservatory} {
		if observatory != nil {
			prefixSelectors(observatory, "subjectSelector", prefix)
			target[key] = observatory
			target["stats"] = map[string]any{}
		}
	}
	return nil
}

func prefixSelectors(object map[string]any, key, prefix string) {
	if selectors, ok := object[key].([]any); ok {
		for i, raw := range selectors {
			if tag, ok := raw.(string); ok {
				selectors[i] = prefix + tag
			}
		}
	}
}

func rewriteStreamDialers(value any, prefix string) {
	switch v := value.(type) {
	case map[string]any:
		if sock, ok := v["sockopt"].(map[string]any); ok {
			if tag, ok := sock["dialerProxy"].(string); ok && tag != "" {
				sock["dialerProxy"] = prefix + tag
			}
		}
		for _, item := range v {
			rewriteStreamDialers(item, prefix)
		}
	case []any:
		for _, item := range v {
			rewriteStreamDialers(item, prefix)
		}
	}
}
