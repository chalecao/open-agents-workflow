"use client";

import Link from "next/link";
import { Button } from "@multica/ui/components/ui/button";
import { MulticaIcon } from "@multica/ui/components/common/multica-icon";
import { useLocale } from "../i18n";

export function MulticaLanding() {
  const { t } = useLocale();

  return (
    <div className="flex min-h-full items-center justify-center px-6 py-16">
      <div className="flex max-w-2xl flex-col items-center gap-8 text-center">
        <Link href="/" className="flex items-center gap-2.5">
          <MulticaIcon className="size-6 text-[#0a0d12]" noSpin />
          <span className="text-[20px] font-semibold tracking-[0.04em] lowercase text-[#0a0d12]">
            multica
          </span>
        </Link>

        <p className="text-[17px] leading-7 text-[#0a0d12]/72 sm:text-[19px]">
          {t.hero.subheading}
        </p>

        <Button
          size="lg"
          render={<Link href="/login" />}
          className="h-11 px-7 text-[15px]"
        >
          {t.hero.cta}
        </Button>
      </div>
    </div>
  );
}
