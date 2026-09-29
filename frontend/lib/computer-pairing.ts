function shellQuote(value: string): string {
  return `'${value.replaceAll("'", `'"'"'`)}'`;
}

function powerShellQuote(value: string): string {
  return `"${value.replaceAll('`', '``').replaceAll('"', '`"')}"`;
}

const defaultInstallerUrl = 'https://raw.githubusercontent.com/solo-agent/solo/master/scripts/install.sh';
const defaultPowerShellInstallerUrl = 'https://raw.githubusercontent.com/solo-agent/solo/master/scripts/install.ps1';

export type PairingTarget = 'unix' | 'windows';

export type PairingCommand = {
  target: PairingTarget;
  labelKey: 'computersTargetMacLinux' | 'computersTargetWindows';
  installed: string;
  fresh: string;
};

// Returns the pairing commands for every supported operating system, in
// display order. Callers pick the target the user is installing on.
export function computerPairingCommands(computerId: string, token: string): PairingCommand[] {
  const server = process.env.NEXT_PUBLIC_API_URL ?? 'http://localhost:8080';
  const shellInstaller = process.env.NEXT_PUBLIC_INSTALL_URL ?? defaultInstallerUrl;
  const powerShellInstaller = process.env.NEXT_PUBLIC_INSTALL_URL_WINDOWS ?? defaultPowerShellInstallerUrl;

  return [
    {
      target: 'unix',
      labelKey: 'computersTargetMacLinux',
      installed: `solo daemon connect --server ${shellQuote(server)} --computer-id ${shellQuote(computerId)} --token ${shellQuote(token)} --profile ${shellQuote(computerId)}`,
      fresh: `curl -fsSL ${shellQuote(shellInstaller)} | bash -s -- connect --server ${shellQuote(server)} --computer-id ${shellQuote(computerId)} --token ${shellQuote(token)}`,
    },
    {
      target: 'windows',
      labelKey: 'computersTargetWindows',
      installed: `solo daemon connect --server ${powerShellQuote(server)} --computer-id ${computerId} --token ${powerShellQuote(token)} --profile ${computerId}`,
      fresh: `& ([scriptblock]::Create((irm ${powerShellQuote(powerShellInstaller)}))) -Connect -Server ${powerShellQuote(server)} -ComputerId ${powerShellQuote(computerId)} -Token ${powerShellQuote(token)}`,
    },
  ];
}
