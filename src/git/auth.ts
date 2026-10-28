/**
 * How a git client authenticates. git speaks HTTP Basic, so the hub's
 * credential arrives as the password and the username is whatever the
 * client felt like sending; the credential itself is the same one every
 * other route takes, identified the same way.
 */
export function basicPassword(header: string | undefined): string | undefined {
  if (header === undefined || !/^basic /i.test(header)) {
    return undefined;
  }
  const encoded = header.slice('basic '.length).trim();
  let decoded: string;
  try {
    decoded = Buffer.from(encoded, 'base64').toString('utf8');
  } catch {
    return undefined;
  }
  const colon = decoded.indexOf(':');
  // A password may hold colons; a header with none is not a credential.
  return colon === -1 ? undefined : decoded.slice(colon + 1);
}

/** The header the credential is identified from, as a Bearer one. */
export function asBearer(header: string | undefined): string | undefined {
  const password = basicPassword(header);
  return password === undefined ? undefined : `Bearer ${password}`;
}
