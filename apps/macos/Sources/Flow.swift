import SwiftUI

/// Flow is the hero: two streams of light entering from the left, braiding
/// together in the middle, leaving as one.
///
/// Every number on screen is a description of the transfer. This is the
/// transfer — particle density per lane is that link's real share, and the
/// whole thing flows at the measured rate, so a fast bonded download visibly
/// races and a stalled one visibly stops.
struct Flow: View {
    let links: [LinkInfo]
    /// Live share of throughput per interface, 0...1, summing to roughly 1.
    let shares: [String: Double]
    /// Total megabits per second, used to set how fast the light travels.
    let mbps: Double
    let active: Bool

    private let particlesPerLane = 26

    var body: some View {
        TimelineView(.animation(minimumInterval: 1.0 / 60.0, paused: !active)) { timeline in
            Canvas { context, size in
                let t = timeline.date.timeIntervalSinceReferenceDate
                draw(in: &context, size: size, time: t)
            }
        }
        .background(
            RoundedRectangle(cornerRadius: 12, style: .continuous)
                .fill(
                    LinearGradient(
                        colors: [Theme.surface, Theme.ground],
                        startPoint: .topLeading, endPoint: .bottomTrailing
                    )
                )
        )
        .overlay(
            RoundedRectangle(cornerRadius: 12, style: .continuous)
                .strokeBorder(Theme.rule, lineWidth: 1)
        )
        .clipShape(RoundedRectangle(cornerRadius: 12, style: .continuous))
    }

    private func draw(in context: inout GraphicsContext, size: CGSize, time: Double) {
        let lanes = links.isEmpty ? [LinkInfo(iface: "none", label: "—", metered: false)] : links
        // Light moves faster when the transfer does, but the mapping is
        // compressed so a 200 Mbps run is lively rather than a blur.
        let speed = 0.06 + min(1.4, pow(max(0, mbps) / 120.0, 0.65)) * 0.5
        let mergeX = size.width * 0.42
        let exitX = size.width

        drawChannel(&context, size: size, mergeX: mergeX)

        for (laneIndex, link) in lanes.enumerated() {
            let share = shares[link.iface] ?? (active ? 0 : 0.5)
            // A link doing nothing still shows a thin trickle, so the lane
            // reads as present-but-idle rather than missing.
            let count = max(2, Int(Double(particlesPerLane) * max(0.08, share)))
            let colour = Theme.colour(for: link.iface, links: links)
            let laneY = laneCentre(laneIndex: laneIndex, count: lanes.count, height: size.height)

            for i in 0 ..< count {
                let offset = Double(i) / Double(count)
                let phase = (time * speed + offset).truncatingRemainder(dividingBy: 1.0)
                let x = phase * exitX

                let y: CGFloat
                if x < mergeX {
                    // Approaching: slide toward the centre line.
                    let k = CGFloat(x / mergeX)
                    y = laneY + (size.height / 2 - laneY) * easeInOut(k)
                } else {
                    // Braided: cross over the centre, each lane in antiphase so
                    // the strands visibly interleave.
                    let k = Double((x - mergeX) / (exitX - mergeX))
                    let swing = sin(k * .pi * 3 + Double(laneIndex) * .pi) * Double(size.height * 0.17)
                    y = size.height / 2 + CGFloat(swing)
                }

                let fade = phase > 0.92 ? (1 - (phase - 0.92) / 0.08) : 1
                let radius: CGFloat = x < mergeX ? 2.6 : 3.2
                let dot = Path(ellipseIn: CGRect(x: x - radius, y: y - radius,
                                                 width: radius * 2, height: radius * 2))
                context.fill(dot, with: .color(colour.opacity(0.85 * fade)))

                // A soft trail, which reads as motion even in a still screenshot.
                let trail = Path(ellipseIn: CGRect(x: x - radius * 3.2, y: y - radius * 0.7,
                                                   width: radius * 3.4, height: radius * 1.4))
                context.fill(trail, with: .color(colour.opacity(0.16 * fade)))
            }
        }
    }

    /// The channel the light travels along: two inlets that converge into one.
    private func drawChannel(_ context: inout GraphicsContext, size: CGSize, mergeX: CGFloat) {
        let lanes = max(1, links.count)
        for laneIndex in 0 ..< lanes {
            let laneY = laneCentre(laneIndex: laneIndex, count: lanes, height: size.height)
            var path = Path()
            path.move(to: CGPoint(x: 0, y: laneY))
            path.addQuadCurve(to: CGPoint(x: mergeX, y: size.height / 2),
                              control: CGPoint(x: mergeX * 0.62, y: laneY))
            context.stroke(path, with: .color(Theme.rule.opacity(0.9)), lineWidth: 1)
        }
        var trunk = Path()
        trunk.move(to: CGPoint(x: mergeX, y: size.height / 2))
        trunk.addLine(to: CGPoint(x: size.width, y: size.height / 2))
        context.stroke(trunk, with: .color(Theme.rule.opacity(0.9)), lineWidth: 1)
    }

    private func laneCentre(laneIndex: Int, count: Int, height: CGFloat) -> CGFloat {
        guard count > 1 else { return height / 2 }
        let usable = height * 0.62
        let top = (height - usable) / 2
        return top + usable * CGFloat(laneIndex) / CGFloat(count - 1)
    }

    private func easeInOut(_ k: CGFloat) -> CGFloat {
        k < 0.5 ? 2 * k * k : 1 - pow(-2 * k + 2, 2) / 2
    }
}

/// Odometer rolls each digit into place instead of snapping, so a rising rate
/// feels like it is climbing.
struct Odometer: View {
    let value: Double
    var size: CGFloat = 62

    private var text: String { String(format: "%.1f", max(0, value)) }

    var body: some View {
        HStack(spacing: 0) {
            ForEach(Array(text.enumerated()), id: \.offset) { _, ch in
                if ch == "." {
                    Text(".").font(.system(size: size, weight: .medium))
                        .frame(width: size * 0.26)
                } else {
                    Digit(value: Int(String(ch)) ?? 0, size: size)
                }
            }
        }
        .monospacedDigit()
    }

    private struct Digit: View {
        let value: Int
        let size: CGFloat

        var body: some View {
            Text("\(value)")
                .font(.system(size: size, weight: .medium))
                .frame(width: size * 0.58)
                .contentTransition(.numericText(countsDown: false))
                .animation(.spring(response: 0.45, dampingFraction: 0.75), value: value)
        }
    }
}

/// Sparkline scrolls the last minute of throughput, so you can see a link drop
/// out or a transfer ramp up rather than only its current value.
struct Sparkline: View {
    let history: [Double]
    let colour: Color

    var body: some View {
        GeometryReader { geo in
            let peak = max(history.max() ?? 1, 1)
            Canvas { context, size in
                guard history.count > 1 else { return }
                let step = size.width / CGFloat(max(1, history.count - 1))

                var line = Path()
                var fill = Path()
                fill.move(to: CGPoint(x: 0, y: size.height))
                for (i, v) in history.enumerated() {
                    let x = CGFloat(i) * step
                    let y = size.height - CGFloat(v / peak) * size.height
                    if i == 0 { line.move(to: CGPoint(x: x, y: y)) } else { line.addLine(to: CGPoint(x: x, y: y)) }
                    fill.addLine(to: CGPoint(x: x, y: y))
                }
                fill.addLine(to: CGPoint(x: size.width, y: size.height))
                fill.closeSubpath()

                context.fill(fill, with: .linearGradient(
                    Gradient(colors: [colour.opacity(0.32), colour.opacity(0.02)]),
                    startPoint: .zero, endPoint: CGPoint(x: 0, y: size.height)
                ))
                context.stroke(line, with: .color(colour), lineWidth: 1.6)
            }
            .frame(width: geo.size.width, height: geo.size.height)
        }
    }
}

/// Pulse makes a completed transfer announce itself once, rather than silently
/// changing a label.
struct Pulse: ViewModifier {
    let trigger: Bool
    @State private var scale: CGFloat = 1

    func body(content: Content) -> some View {
        content
            .scaleEffect(scale)
            .onChange(of: trigger) { _, now in
                guard now else { return }
                withAnimation(.spring(response: 0.28, dampingFraction: 0.4)) { scale = 1.06 }
                DispatchQueue.main.asyncAfter(deadline: .now() + 0.3) {
                    withAnimation(.spring(response: 0.5, dampingFraction: 0.7)) { scale = 1 }
                }
            }
    }
}

extension View {
    func pulse(on trigger: Bool) -> some View { modifier(Pulse(trigger: trigger)) }
}
