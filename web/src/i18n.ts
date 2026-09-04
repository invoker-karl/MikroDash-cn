/** Translate text that is not rendered into the document, such as a native dialog. */
export function tr(source: string): string {
  const runtime = (window as Window & {
    MikroDashI18n?: { t?: (value: string, context?: string) => string };
  }).MikroDashI18n;
  return runtime?.t ? runtime.t(source, 'native-dialog') : source;
}
