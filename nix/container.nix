{
  lib,
  pkgs,
  callPackage,
  runCommand,
  buildEnv,
  coopr,
  nix2container,
  sourceUrl,
  aardvark-dns,
  crun,
  e2fsprogs,
  fuse-overlayfs,
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
  containerCrun = crun.override { withLibkrun = false; };
  runtimeTools = [
    aardvark-dns
    containerCrun
    e2fsprogs
    fuse-overlayfs
    gitMinimal
    gnupg
    netavark
    openssh
    passt
    slirp4netns
    util-linux
    xz
  ];
  licenses = callPackage ./container-licenses.nix {
    inherit pkgs coopr;
    runtimeRoots = runtimeTools ++ [
      coopr
      uidmapTools
      cacert
      root
    ];
    roots = runtimeTools ++ [
      shadow
      cacert
      coopr
      pkgs.gpgme
      pkgs.libseccomp
      pkgs.stdenv.cc.libc
      pkgs.stdenv.cc.cc.lib
      pkgs.iproute2
      pkgs.mailcap
      pkgs.dns-root-data
      pkgs.gnutar
      pkgs.gzip
      pkgs.gnused
      pkgs.coreutils
      pkgs.gnugrep
      pkgs.glibc.libgcc
      pkgs.libidn2.out
      pkgs.iana-etc
    ];
  };
  uidmapTools = runCommand "coopr-uidmap-tools" { } ''
    mkdir -p "$out/bin"
    cp ${shadow}/bin/newuidmap "$out/bin/newuidmap"
    cp ${shadow}/bin/newgidmap "$out/bin/newgidmap"
  '';
  root = runCommand "coopr-container-config" { } ''
    mkdir -p "$out"/{work,tmp,root,var/lib/coopr,home/user/.local/share,run/coopr,run/user/1000,etc/containers,usr/bin}
    printf 'root:x:0:0::/root:/bin/false\nuser:x:1000:1000::/home/user:/bin/false\n' > "$out/etc/passwd"
    printf 'root:x:0:\nuser:x:1000:\n' > "$out/etc/group"
    printf 'root:1:65535\nuser:1:999\nuser:1001:64536\n' > "$out/etc/subuid"
    printf 'root:1:65535\nuser:1:999\nuser:1001:64536\n' > "$out/etc/subgid"
    cp ${uidmapTools}/bin/newuidmap "$out/usr/bin/newuidmap"
    cp ${uidmapTools}/bin/newgidmap "$out/usr/bin/newgidmap"
    cat > "$out/etc/containers/policy.json" <<'POLICY'
    {"default":[{"type":"insecureAcceptAnything"}]}
    POLICY
    cat > "$out/etc/containers/containers.conf" <<'CONFIG'
    [engine]
    helper_binaries_dir = ["/bin"]
    CONFIG
    cat > "$out/etc/containers/storage.conf" <<'STORAGE'
    [storage]
    driver = "overlay"
    runroot = "/run/containers/storage"
    graphroot = "/var/lib/containers/storage"

    [storage.options.overlay]
    mount_program = "${fuse-overlayfs}/bin/fuse-overlayfs"
    mountopt = "nodev,fsync=0"
    STORAGE
  '';
  runtime = buildEnv {
    name = "coopr-image-root";
    paths = runtimeTools ++ [
      coopr
      licenses.licenseBundle
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
      regex = "/(home|run|run/user|var|var/lib|var/lib/coopr|etc|etc/containers|usr|usr/bin)$";
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
      regex = "/(root|run/coopr)$";
      mode = "0700";
    }
    {
      regex = "/(work|tmp)$";
      mode = "1777";
    }
    {
      regex = "/etc/(passwd|group|subuid|subgid|containers/(policy\\.json|containers\\.conf|storage\\.conf))$";
      mode = "0644";
    }
    {
      regex = "/usr/bin/new(uid|gid)map$";
      mode = "4755";
    }
  ];
in
(nix2container.buildImage {
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
    User = "0:0";
    WorkingDir = "/work";
    Entrypoint = [ "${coopr}/bin/coopr" ];
    Labels."org.opencontainers.image.version" = coopr.version;
    Labels."org.opencontainers.image.source" = sourceUrl;
    Env = [
      "PATH=/usr/bin:${lib.makeBinPath runtimeTools}"
      "HOME=/root"
      "USER=root"
      "XDG_DATA_HOME=/var/lib"
      "XDG_CACHE_HOME=/var/lib/coopr/cache"
      "XDG_RUNTIME_DIR=/run/coopr"
      "BUILDAH_ISOLATION=chroot"
      "TMPDIR=/tmp"
      "SSL_CERT_FILE=${cacert}/etc/ssl/certs/ca-bundle.crt"
    ];
    Volumes = {
      "/var/lib" = { };
    };
  };
})
// {
  inherit (licenses) licenseBundle licenseSources candidateOutputs;
  licensePackages = licenses.packages;
  runtimeRoots = [
    runtime
    root
  ];
  knownGeneratedOutputs = map (path: builtins.unsafeDiscardStringContext (toString path)) [
    runtime
    root
    uidmapTools
    licenses.licenseBundle
  ];
}
