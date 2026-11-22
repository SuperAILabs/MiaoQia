# Sourced by the VPS helper scripts. Host identity and access stay in the
# untracked config/local/vps.env (see config/local/vps.env.example);
# variables already set in the shell win.
vps_env_file=${MIAOQIA_VPS_ENV_FILE:-$root_dir/config/local/vps.env}
if [ -f "$vps_env_file" ]; then
  while IFS='=' read -r name value; do
    case $name in
      MIAOQIA_VPS_HOST|MIAOQIA_VPS_SSH_USER|MIAOQIA_VPS_SSH_KEY)
        [ -n "${!name:-}" ] || export "$name=$value" ;;
    esac
  done < "$vps_env_file"
fi
: "${MIAOQIA_VPS_HOST:?set MIAOQIA_VPS_HOST in $vps_env_file or the shell}"
: "${MIAOQIA_VPS_SSH_USER:?set MIAOQIA_VPS_SSH_USER in $vps_env_file or the shell}"
key_path=${MIAOQIA_VPS_SSH_KEY:-$HOME/.ssh/id_rsa}
ssh_user=$MIAOQIA_VPS_SSH_USER
ssh_host=$MIAOQIA_VPS_HOST
