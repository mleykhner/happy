self: { config, lib, pkgs, ... }:
let
  cfg = config.services.happy-gateway;
  inherit (lib) mkEnableOption mkOption types mkIf;
in {
  options.services.happy-gateway = {
    enable = mkEnableOption "Happy WireGuard gateway";
    package = mkOption { type = types.package; default = self.packages.${pkgs.stdenv.hostPlatform.system}.happy; };
    listen = mkOption { type = types.str; default = "127.0.0.1:8080"; };
    passwordFile = mkOption { type = types.str; description = "Absolute runtime path to an admin password, outside the Nix store"; };
    domain = mkOption { type = types.nullOr types.str; default = null; description = "Optional nginx HTTPS domain"; };
    openFirewall = mkOption { type = types.bool; default = false; };
    wireGuardPort = mkOption { type = types.port; default = 51820; description = "Firewall port; match the port set in the panel"; };
  };
  config = mkIf cfg.enable {
    assertions = [
      { assertion = lib.hasPrefix "/" cfg.passwordFile && !(lib.hasPrefix "/nix/store/" cfg.passwordFile); message = "Happy passwordFile must be an absolute runtime path outside /nix/store"; }
      { assertion = cfg.domain == null || cfg.listen == "127.0.0.1:8080"; message = "Happy nginx integration requires listen = 127.0.0.1:8080"; }
    ];
    systemd.services.happy = {
      description = "Happy WireGuard to Xray gateway";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      environment = {
        HAPPY_DATA_DIR = "/var/lib/happy";
        HAPPY_LISTEN = cfg.listen;
        HAPPY_PASSWORD_FILE = "%d/admin-password";
      } // lib.optionalAttrs (cfg.domain != null) {
        HAPPY_ORIGIN = "https://${cfg.domain}";
        HAPPY_SECURE_COOKIE = "true";
      };
      serviceConfig = {
        ExecStart = "${cfg.package}/bin/happy serve";
        DynamicUser = true;
        StateDirectory = "happy";
        StateDirectoryMode = "0700";
        LoadCredential = "admin-password:${cfg.passwordFile}";
        Restart = "on-failure";
        RestartSec = 3;
        KillMode = "control-group";
        UMask = "0077";
        NoNewPrivileges = true;
        PrivateTmp = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectControlGroups = true;
        RestrictSUIDSGID = true;
        CapabilityBoundingSet = "";
      };
    };
    networking.firewall.allowedUDPPorts = lib.mkIf cfg.openFirewall [ cfg.wireGuardPort ];
    services.nginx = mkIf (cfg.domain != null) {
      enable = true;
      virtualHosts.${cfg.domain} = {
        forceSSL = true;
        enableACME = true;
        locations."/".proxyPass = "http://127.0.0.1:8080";
      };
    };
  };
}
