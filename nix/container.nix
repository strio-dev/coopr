{
  lib,
  runCommand,
  buildEnv,
  coopr,
  nix2container,
  aardvark-dns,
  crun,
  e2fsprogs,
  gitMinimal,
  openssh,
  gnupg,
  netavark,
  passt,
  slirp4netns,
  shadow,
  util-linux,
  xz,
  cacert,
}:
let
  runtimeTools = [
    aardvark-dns
    crun
    e2fsprogs
    gitMinimal
    gnupg
    netavark
    openssh
    passt
    slirp4netns
    util-linux
    xz
  ];
  uidmapTools = runCommand "coopr-uidmap-tools" { } ''
    mkdir -p "$out/bin"
    cp ${shadow}/bin/newuidmap "$out/bin/newuidmap"
    cp ${shadow}/bin/newgidmap "$out/bin/newgidmap"
  '';
  root = runCommand "coopr-container-config" { } ''
    mkdir -p "$out"/{work,tmp,home/user/.local/share,run/user/1000,etc/containers,usr/bin}
    printf 'user:x:1000:1000::/home/user:/bin/false\n' > "$out/etc/passwd"
    printf 'user:x:1000:\n' > "$out/etc/group"
    printf 'user:1:999\nuser:1001:64536\n' > "$out/etc/subuid"
    printf 'user:1:999\nuser:1001:64536\n' > "$out/etc/subgid"
    cp ${uidmapTools}/bin/newuidmap "$out/usr/bin/newuidmap"
    cp ${uidmapTools}/bin/newgidmap "$out/usr/bin/newgidmap"
    cat > "$out/etc/containers/policy.json" <<'POLICY'
    {"default":[{"type":"insecureAcceptAnything"}]}
    POLICY
    cat > "$out/etc/containers/containers.conf" <<'CONFIG'
    [engine]
    helper_binaries_dir = ["/bin"]
    CONFIG
  '';
  runtime = buildEnv {
    name = "coopr-image-root";
    paths = runtimeTools ++ [
      coopr
      uidmapTools
      cacert
    ];
    pathsToLink = [
      "/bin"
      "/etc"
      "/share/licenses/coopr"
    ];
  };
  permissions = [
    {
      regex = "/(home|run|run/user|etc|etc/containers|usr|usr/bin)$";
      mode = "0755";
    }
    {
      regex = "/home/user(/.*)?$";
      mode = "0755";
      uid = 1000;
      gid = 1000;
    }
    {
      regex = "/run/user/1000$";
      mode = "0700";
      uid = 1000;
      gid = 1000;
    }
    {
      regex = "/(work|tmp)$";
      mode = "1777";
    }
    {
      regex = "/etc/(passwd|group|subuid|subgid|containers/(policy\\.json|containers\\.conf))$";
      mode = "0644";
    }
    {
      regex = "/usr/bin/new(uid|gid)map$";
      mode = "4755";
    }
  ];
in
nix2container.buildImage {
  name = "coopr";
  tag = "nix";
  copyToRoot = [
    runtime
    root
  ];
  # Shared directories need identical tar-header rules on both root trees.
  perms =
    lib.concatMap
      (
        path:
        map (
          permission:
          permission
          // {
            inherit path;
            regex = "^(${root}|${runtime})${permission.regex}";
          }
        ) permissions
      )
      [
        runtime
        root
      ];
  config = {
    User = "1000:1000";
    WorkingDir = "/work";
    Entrypoint = [ "${coopr}/bin/coopr" ];
    Labels."org.opencontainers.image.version" = coopr.version;
    Env = [
      "PATH=/usr/bin:${lib.makeBinPath runtimeTools}"
      "HOME=/home/user"
      "USER=user"
      "XDG_DATA_HOME=/home/user/.local/share"
      "XDG_CACHE_HOME=/home/user/.local/share/cache"
      "XDG_RUNTIME_DIR=/run/user/1000"
      "TMPDIR=/tmp"
      "SSL_CERT_FILE=${cacert}/etc/ssl/certs/ca-bundle.crt"
    ];
    Volumes = {
      "/home/user/.local/share" = { };
    };
  };
}
